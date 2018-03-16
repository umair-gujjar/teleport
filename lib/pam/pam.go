// +build pam,cgo

package pam

// #cgo LDFLAGS: -ldl
// #include <stdio.h>
// #include <stdlib.h>
// #include <string.h>
// #include <unistd.h>
// #include <dlfcn.h>
// #include <security/pam_appl.h>
// extern char *library_name();
// extern char* readCallback(int, int);
// extern void writeCallback(int n, int s, char* c);
// extern struct pam_conv *make_pam_conv(int);
// extern int _pam_start(void *, const char *, const char *, const struct pam_conv *, pam_handle_t **);
// extern int _pam_end(void *, pam_handle_t *, int);
// extern int _pam_authenticate(void *, pam_handle_t *, int);
// extern int _pam_acct_mgmt(void *, pam_handle_t *, int);
// extern int _pam_open_session(void *, pam_handle_t *, int);
// extern int _pam_close_session(void *, pam_handle_t *, int);
// extern const char *_pam_strerror(void *, pam_handle_t *, int);
import "C"

import (
	"bufio"
	"bytes"
	"io"
	"sync"
	"syscall"
	"unsafe"

	"github.com/gravitational/teleport"
	"github.com/gravitational/trace"

	"github.com/sirupsen/logrus"
)

var log = logrus.WithFields(logrus.Fields{
	trace.Component: teleport.ComponentPAM,
})

// handler is used to register and find instances of *PAM at the package level
// to enable callbacks from C code.
type handler interface {
	// writeStream will write to the output stream (stdout or stderr or
	// equivlient).
	writeStream(int, string) (int, error)

	// readStream will read from the input stream (stdin or equivlient).
	readStream(bool) (string, error)
}

var handlerMu sync.Mutex
var handlerCount int
var handlers map[int]handler = make(map[int]handler)

//export writeCallback
func writeCallback(index C.int, stream C.int, s *C.char) {
	handlerMu.Lock()
	defer handlerMu.Unlock()

	handle, ok := handlers[int(index)]
	if !ok {
		log.Errorf("Unable to write to output stream, no handler for index %v found", int(index))
		return
	}

	// To prevent poorly written PAM modules from sending more data than they
	// should, cap strings to the maximum message size that PAM allows.
	str := C.GoStringN(s, C.int(C.strnlen(s, C.PAM_MAX_MSG_SIZE)))

	// Write to the stream (typically stdout or stderr or equivlient).
	handle.writeStream(int(stream), str)
}

//export readCallback
func readCallback(index C.int, e C.int) *C.char {
	handlerMu.Lock()
	defer handlerMu.Unlock()

	handle, ok := handlers[int(index)]
	if !ok {
		log.Errorf("Unable to read from input stream, no handler for index %v found", int(index))
		return nil
	}

	var echo bool
	if e == 1 {
		echo = true
	}

	// Read from the stream (typically stdin or equivlient).
	s, err := handle.readStream(echo)
	if err != nil {
		log.Errorf("Unable to read from input stream: %v", err)
		return nil
	}

	// Return one less than PAM_MAX_RESP_SIZE to prevent a Teleport user from
	// sending more than a PAM module can handle and to allow space for \0.
	//
	// Note: The function C.CString allocates memory using malloc. The memory is
	// not released in Go code because the caller of the callback function (PAM
	// module) will release it. C.CString will null terminate s.
	n := int(C.PAM_MAX_RESP_SIZE)
	if len(s) > n-1 {
		return C.CString(s[:n-1])
	}
	return C.CString(s)
}

// registerHandler will register a instance of *PAM with the package level
// handlers to support callbacks from C.
func registerHandler(p *PAM) int {
	handlerMu.Lock()
	defer handlerMu.Unlock()

	// The make_pam_conv function allocates struct pam_conv on the heap. It will
	// be released by Close function.
	handlerCount = handlerCount + 1
	p.conv = C.make_pam_conv(C.int(handlerCount))
	handlers[handlerCount] = p

	return handlerCount
}

// unregisterHandler will remove the PAM handle from the package level map
// once no more C callbacks can come back.
func unregisterHandler(handlerIndex int) {
	handlerMu.Lock()
	defer handlerMu.Unlock()

	delete(handlers, handlerIndex)
}

var buildHasPAM bool = true
var systemHasPAM bool = false

// pamHandle is a opaque handle to the libpam object.
var pamHandle unsafe.Pointer

func init() {
	// Obtain a handle to the PAM library at runtime. The package level variable
	// SystemHasPAM is updated to true if a handle is obtained.
	//
	// Note: Since this handle is needed the entire time Teleport runs, dlclose()
	// is never called. The OS will cleanup when the process exits.
	pamHandle = C.dlopen(C.library_name(), C.RTLD_NOW)
	if pamHandle != nil {
		systemHasPAM = true
	}
}

// PAM is used to create a PAM context and initiate PAM transactions to check
// the users account and open/close a session.
type PAM struct {
	// pamh is a handle to the PAM transaction state.
	pamh *C.pam_handle_t

	// conv is the PAM conversation function for communication between
	// Teleport and the PAM module.
	conv *C.struct_pam_conv

	// retval holds the value returned by the last PAM call.
	retval C.int

	// stdin is the input stream which the conversation function will use to
	// obtain data from the user.
	stdin io.Reader

	// stdout is the output stream which the conversation function will use to
	// show data to the user.
	stdout io.Writer

	// stderr is the output stream which the conversation function will use to
	// report errors to the user.
	stderr io.Writer

	// service_name is the name of the PAM policy to use.
	service_name *C.char

	// user is the name of the target user.
	user *C.char

	// handlerIndex is the index to the package level handler map.
	handlerIndex int
}

// Open creates a PAM context and initiates a PAM transaction to check the
// account and then opens a session.
func Open(config *Config) (*PAM, error) {
	if config == nil {
		return nil, trace.BadParameter("PAM configuration is required.")
	}

	p := &PAM{
		pamh:   nil,
		stdin:  config.Stdin,
		stdout: config.Stdout,
		stderr: config.Stderr,
	}

	// Both config.ServiceName and config.Username convert between Go strings to
	// C strings. Since the C strings are allocated on the heap in Go code, this
	// memory must be released (and will be on the call to the Close method).
	p.service_name = C.CString(config.ServiceName)
	p.user = C.CString(config.Username)

	// C code does not know that this PAM context exists. To ensure the
	// conversation function can get messages to the right context, a handle
	// registry at the package level is created (handlers). Each instance of the
	// PAM context has it's own handle which is used to communicate between C
	// and a instance of a PAM context.
	p.handlerIndex = registerHandler(p)

	// Create and initialize a PAM context. The pam_start function will
	// allocate pamh if needed and the pam_end function will release any
	// allocated memory.
	p.retval = C._pam_start(pamHandle, p.service_name, p.user, p.conv, &p.pamh)
	if p.retval != C.PAM_SUCCESS {
		return nil, p.codeToError(p.retval)
	}

	// Check that the *nix account is valid. Checking a account varies based off
	// the PAM modules used in the account stack. Typically this consists of
	// checking if the account is expired or has access restrictions.
	//
	// Note: This function does not perform any authentication!
	retval := C._pam_acct_mgmt(pamHandle, p.pamh, 0)
	if retval != C.PAM_SUCCESS {
		return nil, p.codeToError(retval)
	}

	// Open a user session. Opening a session varies based off the PAM modules
	// used in the "session" stack. Opening a session typically consists of
	// printing the MOTD, mounting a home directory, updating auth.log.
	p.retval = C._pam_open_session(pamHandle, p.pamh, 0)
	if p.retval != C.PAM_SUCCESS {
		return nil, p.codeToError(p.retval)
	}

	return p, nil
}

// Close will close the session, the PAM context, and release any allocated
// memory.
func (p *PAM) Close() error {
	// Close the PAM session. Closing a session can entail anything from
	// unmounting a home directory and updating auth.log.
	p.retval = C._pam_close_session(pamHandle, p.pamh, 0)
	if p.retval != C.PAM_SUCCESS {
		return p.codeToError(p.retval)
	}

	// Terminate the PAM transaction.
	retval := C._pam_end(pamHandle, p.pamh, p.retval)
	if retval != C.PAM_SUCCESS {
		return p.codeToError(retval)
	}

	// Release the memory allocated for the conversation function.
	C.free(unsafe.Pointer(p.conv))

	// Release strings that were allocated when opening the PAM context.
	C.free(unsafe.Pointer(p.service_name))
	C.free(unsafe.Pointer(p.user))

	// Unregister handler index at the package level.
	unregisterHandler(p.handlerIndex)

	return nil
}

// writeStream will write to the output stream (stdout or stderr or
// equivlient).
func (p *PAM) writeStream(stream int, s string) (int, error) {
	writer := p.stdout
	if stream == syscall.Stderr {
		writer = p.stderr
	}

	// Replace \n with \r\n so the message correctly aligned.
	n, err := writer.Write(bytes.Replace([]byte(s), []byte("\n"), []byte("\r\n"), -1))
	if err != nil {
		return n, err
	}

	return n, nil
}

// readStream will read from the input stream (stdin or equivlient).
// TODO(russjones): At some point in the future if this becomes an issue, we
// should consider supporting echo = false.
func (p *PAM) readStream(echo bool) (string, error) {
	reader := bufio.NewReader(p.stdin)
	text, err := reader.ReadString('\n')
	if err != nil {
		return "", trace.Wrap(err)
	}

	return text, nil
}

// codeToError returns a human readable string from the PAM error.
func (p *PAM) codeToError(returnValue C.int) error {
	// Error strings are not allocated on the heap, so memory does not need
	// released.
	err := C._pam_strerror(pamHandle, p.pamh, returnValue)
	if err != nil {
		return trace.BadParameter(C.GoString(err))
	}

	return nil
}

// BuildHasPAM returns true if the binary was build with support for PAM
// compiled in.
func BuildHasPAM() bool {
	return buildHasPAM
}

// SystemHasPAM returns true if the PAM library exists on the system.
func SystemHasPAM() bool {
	return systemHasPAM
}
