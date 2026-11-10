//go:build linux

package service

import (
	"net"
	"os"
	"strings"
)

func Notify(value string) error {
	address := os.Getenv("NOTIFY_SOCKET")
	if address == "" {
		return nil
	}
	if strings.HasPrefix(address, "@") {
		address = "\x00" + address[1:]
	}
	conn, e := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: address, Net: "unixgram"})
	if e != nil {
		return e
	}
	defer conn.Close()
	_, e = conn.Write([]byte(value))
	return e
}
