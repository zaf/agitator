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
	"log/syslog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	server     = iota
	client     = iota
	agiEnvMax  = 150
	agiEnvSize = 512
	wildCard   = "*"
	agiPort    = ":4573"
	confPath   = "/etc/agitator.conf"
)

var (
	confFile    = flag.String("conf", confPath, "Configuration file")
	rtable      RouteTable
	dialTimeout time.Duration
	climit      int
	debug       bool
)

// AgiSession struct
type AgiSession struct {
	ClientCon net.Conn
	ServerCon net.Conn
	Request   *url.URL
	Server    *Server
}

// Config file struct
type Config struct {
	Listen  string
	Port    int
	Timeout int
	Conlim  int
	Log     string
	Debug   bool
	Route   map[string]struct {
		Hosts []string
		Mode  string
	}
}

// RouteTable holds the routing table
type RouteTable map[string]*Path

// Path struct holds a list of hosts and the routing mode
type Path struct {
	sync.RWMutex
	Hosts []*Server
	Mode  string
}

// Server struct holds the server address, priority and the number of active sessions
type Server struct {
	sync.RWMutex
	Host  string
	Count int
}

// ByActive implements sort.Interface for []*Server based on the Count field
type ByActive []*Server

func (s ByActive) Len() int {
	return len(s)
}

func (s ByActive) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s ByActive) Less(i, j int) bool {
	s[i].RLock()
	s[j].RLock()
	res := s[i].Count < s[j].Count
	s[i].RUnlock()
	s[j].RUnlock()
	return res
}

func main() {
	flag.Parse()

	// Parse Config file
	var config Config
	_, err := toml.DecodeFile(*confFile, &config)
	if err != nil {
		log.Fatal(err)
	}

	// Setup logging
	if config.Log == "syslog" {
		logwriter, err := syslog.New(syslog.LOG_NOTICE, "agitator")
		if err == nil {
			log.SetOutput(logwriter)
		}
	}

	// Set some settings as global vars
	dialTimeout = time.Duration(float64(config.Timeout)) * time.Second
	climit = config.Conlim
	debug = config.Debug

	// Generate routing table from config file data
	rtable = genRtable(config)
	if len(rtable) == 0 {
		log.Fatal("No routes specified")
	}

	wg := new(sync.WaitGroup)
	// Handle signals
	var shutdown int32
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGHUP)
	wg.Add(1)
	go sigHandle(sigChan, &shutdown, wg)

	// Create a listener and start a new goroutine for each connection.
	addr := config.Listen + ":" + strconv.Itoa(config.Port)
	log.Printf("Starting AGItator proxy on %v\n", addr)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for atomic.LoadInt32(&shutdown) == 0 {
			conn, err := ln.Accept()
			if err != nil {
				log.Println(err)
				continue
			}
			if debug {
				log.Printf("%v: Connected to %v\n", conn.RemoteAddr(), conn.LocalAddr())
			}
			wg.Add(1)
			go connHandle(conn, wg)
		}
	}()

	wg.Wait()
	return
}

// Generate Routing table from config data
func genRtable(conf Config) RouteTable {
	table := make(RouteTable, len(conf.Route))
	for path, app := range conf.Route {
		if len(app.Hosts) == 0 {
			log.Println("No routes for", path)
			continue
		}
		p := new(Path)
		switch app.Mode {
		case "", "failover":
			p.Mode = "failover"
		case "balance":
			p.Mode = "balance"
		default:
			log.Println("Invalid mode for", path)
			continue
		}
		p.Hosts = make([]*Server, 0, len(app.Hosts))
		for _, host := range app.Hosts {
			if strings.Index(host, ":") == -1 || strings.Index(host, "]") == len(host)-1 {
				// Add default port to host string if not present
				host += agiPort
			}
			s := new(Server)
			s.Host = host
			p.Hosts = append(p.Hosts, s)
		}
		table[path] = p
	}
	return table
}

// Connection handler. Find route, connect to remote server and relay data.
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
	defer sess.ServerCon.Close()
	updateCount(sess.Server, 1)

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
	updateCount(sess.Server, -1)
	return
}

// Read the AGI environment, return it and parse the agi_request url.
func (s *AgiSession) parseEnv() ([]byte, error) {
	var req string
	var err error
	agiEnv := make([]byte, 0, agiEnvSize)
	buf := bufio.NewReader(s.ClientCon)

	// Read the AGI enviroment, store all vars in agiEnv except 'agi_request'.
	// Request is stored separately for parsing and further processing.
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
		err = fmt.Errorf("%v: Non valid AGI request", s.ClientCon.RemoteAddr())
	} else {
		s.Request, err = url.Parse(req)
	}
	return agiEnv, err
}

// Route based on reguest path
func (s *AgiSession) route() error {
	var err error
	client := s.ClientCon.RemoteAddr()
	path := strings.TrimPrefix(s.Request.Path, "/")

	// Find route
	if _, ok := rtable[path]; !ok {
		if _, ok = rtable[wildCard]; !ok {
			return fmt.Errorf("%v: No route found for %s", client, path)
		}
		path = wildCard
	}
	if debug {
		log.Printf("%v: Found route\n", client)
	}
	// Load Balance mode: Sort servers by number of active sessions
	if rtable[path].Mode == "balance" {
		rtable[path].Lock()
		sort.Sort(ByActive(rtable[path].Hosts))
		rtable[path].Unlock()
	}

	// Find available servers and connect
	for i := 0; i < len(rtable[path].Hosts); i++ {
		server := rtable[path].Hosts[i]
		server.RLock()
		if climit > 0 && server.Count >= climit {
			server.RUnlock()
			log.Printf("%v: Reached connections limit in %s\n", client, server.Host)
			continue
		}
		server.RUnlock()
		s.ServerCon, err = net.DialTimeout("tcp", server.Host, dialTimeout)
		if err == nil {
			s.Request.Host = server.Host
			s.Server = server
			return err
		} else if debug {
			log.Printf("%v: Failed to connect to %s\n", client, server.Host)
		}
	}

	//No servers found
	return fmt.Errorf("%v: Unable to connect to any server", client)
}

// Update active session counter
func updateCount(srv *Server, i int) {
	srv.Lock()
	srv.Count += i
	srv.Unlock()
}

// Signal handler. SIGINT exits cleanly, SIGHUP reloads config.
func sigHandle(schan <-chan os.Signal, s *int32, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		signal := <-schan
		switch signal {
		case os.Interrupt:
			log.Printf("Received %v, Waiting for remaining sessions to end to exit.\n", signal)
			atomic.StoreInt32(s, 1)
			return
		case syscall.SIGHUP:
			log.Printf("Received %v, reloading routing rules from config file\n", signal)
			var config Config
			_, err := toml.DecodeFile(*confFile, &config)
			if err != nil {
				log.Println("Failed to read config file:", err)
				break
			}
			// Generate routing table from config file data
			table := genRtable(config)
			if len(table) == 0 {
				log.Println("No routes specified, using old config data.")
				break
			}
			rtable = table

		}
	}
}
