package steps

import (
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/recipe"
)

// FromRecipe turns the steps of a recipe into pipeline entries.
func FromRecipe(rec *recipe.Recipe) []pipeline.Entry {
	var out []pipeline.Entry
	cloned := false
	for _, s := range rec.Steps {
		e := pipeline.Entry{Name: s.Type, ID: s.ID, Family: s.Family}
		switch s.Type {
		case "reset":
			e.Step = Reset{Label: s.Label}
		case "boot":
			e.Step = Boot{}
		case "viewer":
			e.Step = Viewer{}
		case "wait-ssh":
			e.Step = WaitSSH{}
		case "verify-guest-password":
			e.Step = VerifyGuestPassword{}
		case "detect-guest":
			e.Step = DetectGuest{}
		case "hostname":
			e.Step = Hostname{}
		case "luks-mark":
			e.Step = LUKSMark{}
		case "autologin":
			e.Step = &Autologin{}
		case "upload":
			e.Step = Upload{Files: s.Files}
		case "wait-session":
			e.Step = WaitSession{}
		case "unlock-keyring":
			e.Step = UnlockKeyring{}
		case "sudo-grant":
			e.Step = &SudoGrant{}
		case "usb-passthrough":
			e.Step = &UsbPassthrough{Label: s.Label}
		case "pinentry":
			e.Step = &Pinentry{Secret: s.Secret, Spec: rec.Secrets[s.Secret]}
		case "run":
			e.Step = Run{Command: s.Command, Secret: s.Secret, Spec: rec.Secrets[s.Secret]}
		case "checks":
			e.Step = Checks{Dir: s.Dir, Command: s.Command}
		case "reboot":
			e.Step = Reboot{}
		case "poweroff":
			e.Step = Poweroff{}
		case "clone":
			e.Step = Clone{}
			cloned = true
		case "generalize":
			e.Step = Generalize{}
		case "snapshot":
			e.Step = &Snapshot{Label: s.Label, OnClone: cloned}
		case "undo":
			e.Step = pipeline.Undo{ID: s.Undo}
		}
		out = append(out, e)
	}
	return out
}
