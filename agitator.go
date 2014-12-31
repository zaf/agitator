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
	"strings"
	"sync"
	"sync/atomic"

	"github.com/BurntSushi/toml"
)

const (
	server = iota
	client = iota
)

// AgiSession struct
type AgiSession struct {
	ClientCon net.Conn
	ServerCon net.Conn
	Request   *url.URL
}

// conf file structs
type Config struct {
	Listen string
	Port   int
	Debug  bool
	App   map[string]Server
}

//
type Server struct {
	Host string
	Port int
}

var config Config

func main() {
	var conf = flag.String("conf", "/etc/agitator.conf", "Configuration file")
	flag.Parse()
	_, err := toml.DecodeFile(*conf, &config)
	if err != nil {
		log.Fatal(err)
	}
	//log.Println(config)
	// Handle interrupts
	var shutdown int32
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)

	// Create a listener and start a new goroutine for each connection.
	addr := net.JoinHostPort(config.Listen, strconv.Itoa(config.Port))
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
			if config.Debug {
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

func connHandle(conn net.Conn, wg *sync.WaitGroup) {
	sess := new(AgiSession)
	sess.ClientCon = conn
	var err error
	defer func() {
		sess.ClientCon.Close()
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
	sess.ServerCon, err = net.Dial("tcp", sess.Request.Host)
	if err != nil {
		log.Println(err)
		return
	}
	defer sess.ServerCon.Close()
	env = append(env, []byte("agi_request: "+sess.Request.String()+"\n\r\n")...)
	sess.ServerCon.Write(env)
	// Relay data between the 2 connections.
	done := make(chan int)
	go func() {
		io.Copy(sess.ServerCon, sess.ClientCon)
		done <- client
	}()
	go func() {
		io.Copy(sess.ClientCon, sess.ServerCon)
		done <- server
	}()
	fin := <-done
	if fin == client {
		sess.ServerCon.Close()
	} else {
		sess.ClientCon.Close()
	}
	<-done
	close(done)
	return
}

// Read the AGI environment, return it and parse the agi_request url.
func (s *AgiSession) parseEnv() ([]byte, error) {
	agiEnv := make([]byte, 0, 512)
	var req string
	var err error
	buf := bufio.NewReader(s.ClientCon)
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
		s.Request, err = url.Parse(req)
	}
	return agiEnv, err
}

// Route based on reguest path
func (s *AgiSession) route() error {
	var err error
	path := strings.TrimPrefix(s.Request.Path, "/")
	if server, ok := config.App[path]; ok {
		s.Request.Host = server.Host + ":" + strconv.Itoa(server.Port)
	} else if server, ok := config.App["*"]; ok {
		s.Request.Host = server.Host + ":" + strconv.Itoa(server.Port)
	} else {
		err = fmt.Errorf("No route found")
	}
	return err
}
