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
	server     = iota
	client     = iota
	agiEnvMax  = 150
	agiEnvSize = 512
	agiPort    = ":4573"
	confPath   = "/etc/agitator.conf"
)

// AgiSession struct
type AgiSession struct {
	ClientCon *net.TCPConn
	ServerCon *net.TCPConn
	Request   *url.URL
}

// RouteTable holds the routing table
type RouteTable map[string]Path

// Config file struct
type Config struct {
	Listen string
	Port   int
	Debug  bool
	Route  map[string]App
}

// App struct
type App struct {
	Hosts []string
	Mode  string
}

// Path struct
type Path struct {
	Hosts []Server
	Mode  string
}

// Server struct
type Server struct {
	Host  string
	Count int
}

var (
	rtable RouteTable
	debug  bool
)

func main() {
	var config Config
	var confFile = flag.String("conf", confPath, "Configuration file")
	flag.Parse()
	_, err := toml.DecodeFile(*confFile, &config)
	if err != nil {
		log.Fatal(err)
	}
	debug = config.Debug

	// Generate Routing table
	rtable = genRtable(config)
	if len(rtable) == 0 {
		log.Fatal("No routes specified")
	}

	// Handle interrupts
	var shutdown int32
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)

	// Create a listener and start a new goroutine for each connection.
	addr := config.Listen + ":" + strconv.Itoa(config.Port)
	tcpaddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Starting AGItator proxy on %v\n", tcpaddr)
	ln, err := net.ListenTCP("tcp", tcpaddr)
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()
	wg := new(sync.WaitGroup)
	go func() {
		for atomic.LoadInt32(&shutdown) == 0 {
			conn, err := ln.AcceptTCP()
			if err != nil {
				log.Println(err)
				continue
			}
			if debug {
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

// Generate Routing table from config data
func genRtable(conf Config) RouteTable {
	table := make(RouteTable)
	for path, app := range conf.Route {
		if len(app.Hosts) == 0 {
			log.Println("No routes for", path)
			continue
		}
		p := Path{}
		p.Mode = app.Mode
		for _, host := range app.Hosts {
			if strings.Index(host, ":") == -1 {
				host += agiPort
			}
			s := Server{host, 0}
			p.Hosts = append(p.Hosts, s)
		}
		table[path] = p
	}
	return table
}

func connHandle(conn *net.TCPConn, wg *sync.WaitGroup) {
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
	defer sess.ServerCon.Close()
	// Send the AGI env to the server.
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
	agiEnv := make([]byte, 0, agiEnvSize)
	var req string
	var err error
	buf := bufio.NewReader(s.ClientCon)
	for i := 0; i <= agiEnvMax; i++ {
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
	var tcpaddr *net.TCPAddr

	path := strings.TrimPrefix(s.Request.Path, "/")
	if debug {
		log.Println("Routing request for:", path)
	}
	dest, ok := rtable[path]
	if ok {
		if debug {
			log.Println("Found route for", path)
		}
	} else {
		dest, ok = rtable["*"]
		if !ok {
			err = fmt.Errorf("No route found")
			return err
		}
		if debug {
			log.Println("Using wildcard route for", path)
		}
	}
	//if dest.Mode == "failover" || dest.Mode == "" {
	for _, server := range dest.Hosts {
		tcpaddr, err = net.ResolveTCPAddr("tcp", server.Host)
		if err != nil {
			continue
		}
		s.ServerCon, err = net.DialTCP("tcp", nil, tcpaddr)
		if err == nil {
			s.Request.Host = server.Host
			break
		} else if debug {
			log.Println("Failed to connect to", tcpaddr, err)
		}
	}
	return err
}
