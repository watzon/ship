// The probe distinguishes a TCP connection failure from fixture startup errors.
package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: probe HOST:PORT")
		os.Exit(2)
	}
	conn, err := net.DialTimeout("tcp", os.Args[1], 2*time.Second)
	if err != nil {
		fmt.Printf("connect_failed: %v\n", err)
		os.Exit(3)
	}
	_ = conn.Close()
	fmt.Println("connected")
}
