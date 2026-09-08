// These endpoints exercise both orders of TCP half-close.
package main

import (
	"crypto/sha256"
	"io"
	"log"
	"net"
)

func main() {
	go serve(":4001", true)
	serve(":4000", false)
}

func serve(address string, serverFirst bool) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatal(err)
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			defer conn.Close()
			if serverFirst {
				if _, err := conn.Write([]byte("ready\n")); err != nil {
					return
				}
				if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
					return
				}
			}
			body, err := io.ReadAll(io.LimitReader(conn, 1024*1024))
			if err != nil {
				return
			}
			if serverFirst {
				log.Printf("server-first received %d bytes sha256=%x", len(body), sha256.Sum256(body))
				return
			}
			_, _ = conn.Write(append([]byte("reply:"), body...))
			_ = conn.(*net.TCPConn).CloseWrite()
		}()
	}
}
