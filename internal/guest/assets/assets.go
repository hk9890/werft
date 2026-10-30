// Package assets holds the generic guest helpers werft copies into a guest.
package assets

import (
	_ "embed"

	"github.com/hk9890/werft/internal/guest"
)

var (
	//go:embed wait-for-package-manager.sh
	WaitForPackageManager []byte
	//go:embed unlock-keyring.js
	UnlockKeyring []byte
	//go:embed pinentry-auto
	PinentryAuto []byte
	// Hostname is a root script that names the guest $1.
	//go:embed hostname.sh
	Hostname string
	// Generalize is a root script that gives a clone named $1 its own
	// identity.
	//go:embed generalize.sh
	Generalize string
)

// Helpers are the files the upload step puts into the guest directory.
func Helpers() []guest.File {
	return []guest.File{
		{Name: "wait-for-package-manager.sh", Data: WaitForPackageManager, Mode: 0o755},
		{Name: "unlock-keyring.js", Data: UnlockKeyring, Mode: 0o755},
		{Name: "pinentry-auto", Data: PinentryAuto, Mode: 0o755},
	}
}
