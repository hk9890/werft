// Package guest is the guest port: run a command in a guest over SSH, with
// data on stdin. Scripts, scripts as root and copying files build on it. A
// secret reaches a guest only as stdin data, never on a command line.
package guest

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Addr is a login in a guest. Host is an IP address, or vsock/<cid> for the
// AF_VSOCK path.
type Addr struct {
	User string
	Host string
}

// VsockSystemd is the first systemd version with SSH over AF_VSOCK, and with
// the ssh.ephemeral-authorized_keys-all credential.
const VsockSystemd = 256

// TCPSystemd is the one systemd version below VsockSystemd that werft serves.
// It imports tmpfiles.extra from SMBIOS, so werft logs in over TCP at the DHCP
// address of the guest.
const TCPSystemd = 255

func (a Addr) String() string { return a.User + "@" + a.Host }

// Shell runs cmd in the guest at addr, with stdin as its input; a nil stdin
// is empty. A command that ran and failed returns an *ExitError.
type Shell interface {
	Run(ctx context.Context, addr Addr, cmd string, stdin io.Reader, stdout, stderr io.Writer) error
}

// ExitError is a guest command that exited non-zero.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("guest command exited with status %d", e.Code) }

// Quote makes s one word for a POSIX shell.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ScriptCommand runs script with sh, with args as its $1... The script
// travels as an argument, so stdin stays free for data such as a secret.
func ScriptCommand(script string, args ...string) string {
	words := []string{"sh -c", Quote(script), "sh"}
	for _, a := range args {
		words = append(words, Quote(a))
	}
	return strings.Join(words, " ")
}

// RootCommand runs script as root through sudo, with args as its $1... sudo
// reads the password from stdin and the script travels as an argument: where
// sudo needs no password it leaves stdin unread, and a script fed through
// stdin would then start with the password. -k makes sudo ask every time, so
// a wrong password cannot hide behind a cached one.
func RootCommand(script string, args ...string) string {
	return "sudo -k -S -p '' " + ScriptCommand(script, args...)
}

// Line is a secret as stdin data: one line.
func Line(secret string) io.Reader {
	return strings.NewReader(secret + "\n")
}

// File is one file or directory to copy into the guest. Name is its path
// inside the target directory. A File with Data is written from memory;
// otherwise Source is a host path, and a directory is copied whole.
type File struct {
	Name   string
	Source string
	Data   []byte
	Mode   fs.FileMode
}

// Copy replaces dir in the guest at addr with files: it empties dir and
// unpacks the files there from one tar stream, over one SSH login. out gets
// the output of the guest command.
func Copy(ctx context.Context, sh Shell, addr Addr, dir string, files []File, out io.Writer) error {
	var stream bytes.Buffer
	if err := WriteTar(&stream, files); err != nil {
		return err
	}
	q := Quote(dir)
	return sh.Run(ctx, addr, fmt.Sprintf("rm -rf %s && mkdir -p %s && tar -x -C %s", q, q, q), &stream, out, out)
}

// WriteTar writes files as one tar stream.
func WriteTar(w io.Writer, files []File) error {
	tw := tar.NewWriter(w)
	for _, f := range files {
		var err error
		if f.Source == "" {
			err = writeEntry(tw, f.Name, f.Mode, f.Data)
		} else {
			err = addTree(tw, f.Source, f.Name)
		}
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

func addTree(tw *tar.Writer, root, name string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entry := filepath.ToSlash(filepath.Join(name, rel))
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: entry + "/", Mode: int64(info.Mode().Perm())})
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is no regular file", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return writeEntry(tw, entry, info.Mode().Perm(), data)
	})
}

func writeEntry(tw *tar.Writer, name string, mode fs.FileMode, data []byte) error {
	if mode == 0 {
		mode = 0o644
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: int64(mode), Size: int64(len(data))}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}
