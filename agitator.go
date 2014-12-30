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
	"strconv"
	"sync"
	"sync/atomic"
)

// Session struct
type AgiSession struct {
	clientCon net.Conn
	serverCon net.Conn
	request   *url.URL
}

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
	// Do config stuff...

	// Handle interrupts
	var shutdown int32
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)

	// Create a listener and start a new goroutine for each connection.
	addr := net.JoinHostPort(*listen, *port)
	log.Printf("Starting AGItator proxy on %v\n", addr)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()
	wg := new(sync.WaitGroup)
	go func() {
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
	}()
	signal := <-sigChan
	log.Printf("Received %v, Waiting for remaining sessions to end to exit.\n", signal)
	atomic.StoreInt32(&shutdown, 1)
	wg.Wait()
	return
}

func connHandle(client net.Conn, wg *sync.WaitGroup) {
	sess := new(AgiSession)
	sess.clientCon = client
	var err error
	defer func() {
		sess.clientCon.Close()
		wg.Done()
	}()

	// Read the AGI Env variables and parse the request url.
	env, err := sess.parseEnv()
	if err != nil {
		log.Println(err)
		return
	}
	// Do the routing
	err = sess.route()
	if err != nil {
		log.Println(err)
		return
	}

	// Dial remote server and send the AGI Env data
	sess.serverCon, err = net.Dial("tcp", sess.request.Host)
	if err != nil {
		log.Println(err)
		return
	}
	defer sess.serverCon.Close()
	env = append(env, []byte("agi_request: "+sess.request.String()+"\n\r\n")...)
	sess.serverCon.Write(env)
	// Relay data between the 2 connections.
	serverDone := make(chan int)
	clientDone := make(chan int)
	go func() {
		io.Copy(sess.serverCon, sess.clientCon)
		clientDone <- 1
	}()
	go func() {
		io.Copy(sess.clientCon, sess.serverCon)
		serverDone <- 1
	}()

	select {
	case <-serverDone:
		sess.clientCon.Close()
		<-clientDone
	case <-clientDone:
		sess.serverCon.Close()
		<-serverDone
	}
	close(serverDone)
	close(clientDone)
	return
}

// Read the AGI environment, return it and parse the agi_request url.
func (s *AgiSession) parseEnv() ([]byte, error) {
	agiEnv := make([]byte, 0, 512)
	var req string
	var err error
	buf := bufio.NewReader(s.clientCon)
	for i := 0; i <= 150; i++ {
		line, err := buf.ReadBytes(10)
		if err != nil || len(line) <= len("\r\n") {
			break
		}
		ind := bytes.IndexByte(line, ':')
		if string(line[:ind]) == "agi_request" && len(line) >= ind+len(": \n") {
			ind += len(": ")
			req = string(line[ind : len(line)-1])
		} else {
			agiEnv = append(agiEnv, line...)
		}
	}
	if req == "" {
		err = fmt.Errorf("Non valid AGI request")
	} else {
		s.request, err = url.Parse(req)
	}
	return agiEnv, err
}

// Shim
func (s *AgiSession) route() error {
	srvHost := "127.0.0.1"
	srvPort := 4545
	var err error
	if s.request.Path == "/myagi" {
		s.request.Host = srvHost + ":" + strconv.Itoa(srvPort)
	} else {
		err = fmt.Errorf("No route found")
	}
	return err
}
