// Package systemd tells systemd the agent is ready and alive, for its watchdog.
package systemd

import (
	"net"
	"os"
)

const (
	Ready    = "READY=1"
	Watchdog = "WATCHDOG=1"
)

// Notifier sends sd_notify messages to Socket. Outside systemd, Socket is empty and
// nothing is sent.
type Notifier struct{ Socket string }

func FromEnv() Notifier { return Notifier{Socket: os.Getenv("NOTIFY_SOCKET")} }

func socketAddr(socket string) *net.UnixAddr {
	if socket[0] == '@' {
		socket = "\x00" + socket[1:] // An abstract socket name starts with a NUL byte.
	}
	return &net.UnixAddr{Name: socket, Net: "unixgram"}
}

func (n Notifier) Notify(state string) error {
	if n.Socket == "" {
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, socketAddr(n.Socket))
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}
