//go:build windows

package main

import (
	"net"
	"testing"
)

func TestPortReservation(t *testing.T) {
	if a, b, err := reserveListeners(18081, 18081); err == nil || a != nil || b != nil {
		t.Fatal("equal ports must fail before binding")
	}
	occupied, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port
	if a, b, err := reserveListeners(0, port); err == nil || a != nil || b != nil {
		t.Fatal("occupied UI port did not abort startup")
	}
	if a, b, err := reserveListeners(port, 0); err == nil || a != nil || b != nil {
		t.Fatal("occupied timer port did not abort startup")
	}
	available, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	freePort := available.Addr().(*net.TCPAddr).Port
	available.Close()
	a, b, err := reserveListeners(freePort, port)
	if err == nil || a != nil || b != nil {
		t.Fatal("expected UI bind failure")
	}
	a, b, err = reserveListeners(freePort, 0)
	if err != nil {
		t.Fatalf("timer port leaked after failure: %v", err)
	}
	for name, listener := range map[string]net.Listener{"timer": a, "management": b} {
		address, ok := listener.Addr().(*net.TCPAddr)
		if !ok || !address.IP.IsUnspecified() {
			t.Errorf("%s listener is not bound to all network interfaces: %v", name, listener.Addr())
		}
	}
	a.Close()
	b.Close()
}
