// +build !pam,cgo

package pam

var buildHasPAM bool = false
var systemHasPAM bool = false

// PAM is used to create a PAM context and initiate PAM transactions to checks
// the users account and open/close a session.
type PAM struct {
}

// Open creates a PAM context and initiates a PAM transaction to check the
// account and then opens a session.
func Open(config *Config) (*PAM, error) {
	return &PAM{}, nil
}

// Close will close the session, the PAM context, and release any allocated
// memory.
func (p *PAM) Close() error {
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
