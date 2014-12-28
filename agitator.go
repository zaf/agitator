/*
	A FastAGI reverse proxy

	Copyright (C) 2014 - 2015, Lefteris Zafiris <zaf.000@gmail.com>

	This program is free software, distributed under the terms of
	the GNU General Public License Version 3. See the LICENSE file
	at the top of the source tree.
*/

package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	conf   = flag.String("conf", "/etc/agitator.conf", "Configuration file")
	debug  = flag.Bool("debug", false, "Print debug information on stderr")
	listen = flag.String("listen", "0.0.0.0", "Listening address")
	port   = flag.String("port", "4573", "Listening server port")
)

func main() {
	flag.Parse()
	if *conf == "" {
		log.Fatal("No configuration file specified.")
	}


	// Handle interrupts
	var shutdown int32
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	go handleHangup(sigChan, &shutdown)

	// Create a listener and start a new goroutine for each connection.
	addr := net.JoinHostPort(*listen, *port)
	log.Printf("Starting AGItator proxy on %v\n", addr)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()
	wg := new(sync.WaitGroup)
	for atomic.LoadInt32(&shutdown) == 0 {
		conn, err := ln.Accept()
		if err != nil {
			log.Println(err)
			continue
		}
		if *debug {
			log.Printf("Connected: %v <-> %v\n", conn.LocalAddr(), conn.RemoteAddr())
		}
		wg.Add(1)
		go connHandle(conn, wg)
	}
	wg.Wait()
}

func connHandle(client net.Conn, wg *sync.WaitGroup) {
	defer func() {
		client.Close()
		wg.Done()
	}()
	// Read the AGI Env variables and get the request string
	request, env := parseEnv(client)

	// Do the routing
	host, err := parseAgiReq(request)
	if err != nil {
		log.Println(err)
		return
	}

	// Dial remote server and send the AGI Env data
	server, err := net.Dial("tcp", host)
	if err != nil {
		log.Println(err)
		return
	}
	defer server.Close()
	server.Write(env)
	// Relay data between the 2 connections.
	serverDone := make(chan int)
	clientDone := make(chan int)
	go func() {
		io.Copy(server, client)
		clientDone <- 1
	}()
	go func() {
		io.Copy(client, server)
		serverDone <- 1
	}()

	select {
	case <-serverDone:
		client.Close()
		close(serverDone)
		<-clientDone
		close(clientDone)
	case <-clientDone:
		server.Close()
		close(clientDone)
		<-serverDone
		close(serverDone)
	}
	return
}

func parseEnv(c net.Conn) (string, []byte) {
	var request string
	agiEnv := make([]byte, 0, 512)
	buf := bufio.NewReader(c)
	for i := 0; i <= 150; i++ {
		line, err := buf.ReadBytes(10)
		if err != nil || len(line) <= len("\r\n") {
			break
		}
		agiEnv = append(agiEnv, line...)
		if request == "" {
			ind := bytes.IndexByte(line, ':')
			if string(line[:ind]) == "agi_request" {
				ind += len(": ")
				request = string(line[ind : len(line)-1])
			}
		}
	}
	agiEnv = append(agiEnv, []byte("\r\n")...)
	return request, agiEnv
}

func parseAgiReq(request string) (string, error) {
	u, err := url.Parse(request)
	if err != nil {
		return "", err
	}
	host := strings.Split(u.Host, ":")
	if host[0] == "127.0.0.1" && u.Path == "/myagi" {
		return host[0] + ":4545", nil
	}
	return "", fmt.Errorf("Unkown request:%s%s", host[0], u.Path)
}

// Stop listening for new connections when received an Interrupt signal
func handleHangup(sch <-chan os.Signal, shutdown *int32) {
	signal := <-sch
	log.Printf("Received %v, Waiting for remaining sessions to end to exit.\n", signal)
	atomic.StoreInt32(shutdown, 1)
}
