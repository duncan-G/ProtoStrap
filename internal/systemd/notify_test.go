package systemd

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

func listen(t *testing.T) (*net.UnixConn, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, path
}

func read(t *testing.T, conn *net.UnixConn) string {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

func TestNotify(t *testing.T) {
	conn, path := listen(t)
	n := Notifier{Socket: path}
	if err := n.Notify(Ready); err != nil {
		t.Fatal(err)
	}
	if got := read(t, conn); got != "READY=1" {
		t.Fatalf("got %q", got)
	}
	if err := n.Notify(Watchdog); err != nil {
		t.Fatal(err)
	}
	if got := read(t, conn); got != "WATCHDOG=1" {
		t.Fatalf("got %q", got)
	}
}

func TestNotifyWithoutSystemd(t *testing.T) {
	if err := (Notifier{}).Notify(Ready); err != nil {
		t.Fatalf("outside systemd, notify does nothing: %v", err)
	}
}

func TestNotifyFromEnv(t *testing.T) {
	_, path := listen(t)
	t.Setenv("NOTIFY_SOCKET", path)
	if FromEnv().Socket != path {
		t.Fatal("socket not read from the environment")
	}
}

func TestAbstractSocketName(t *testing.T) {
	if got := socketAddr("@/org/freedesktop/systemd1/notify").Name; got != "\x00/org/freedesktop/systemd1/notify" {
		t.Fatalf("got %q", got)
	}
}

func TestNotifyMissingSocket(t *testing.T) {
	n := Notifier{Socket: filepath.Join(t.TempDir(), "gone.sock")}
	if err := n.Notify(Ready); err == nil {
		t.Fatal("expected an error")
	}
}
