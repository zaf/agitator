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

var (
	rtable RouteTable
	climit int
	debug  bool
)

// AgiSession struct
type AgiSession struct {
	ClientCon *net.TCPConn
	ServerCon *net.TCPConn
	Request   *url.URL
}

// Config file struct
type Config struct {
	Listen string
	Port   int
	Conlim int
	Log    string
	Debug  bool
	Route  map[string]struct {
		Hosts []string
		Mode  string
	}
}

// RouteTable holds the routing table
type RouteTable map[string]*Path

// Path struct holds a list of hosts and the routing mode
type Path struct {
	Hosts map[string]*SrvPref
	Mode  string
}

// SrvPref holds the priority and the number of active sessions
type SrvPref struct {
	sync.RWMutex
	Priority int
	Count    int
}

// Server struct holds the server address, priority and the number of active sessions
type Server struct {
	Host     string
	Priority int
	Count    int
}

// PerActive implements sort.Interface for []Server based on the Count field
type PerActive []Server

func (s PerActive) Len() int {
	return len(s)
}

func (s PerActive) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s PerActive) Less(i, j int) bool {
	return s[i].Count < s[j].Count
}

// PerPriority implements sort.Interface for []Server based on the Priority field
type PerPriority []Server

func (s PerPriority) Len() int {
	return len(s)
}

func (s PerPriority) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s PerPriority) Less(i, j int) bool {
	return s[i].Priority < s[j].Priority
}

func main() {
	var config Config
	var confFile = flag.String("conf", confPath, "Configuration file")
	flag.Parse()
	_, err := toml.DecodeFile(*confFile, &config)
	if err != nil {
		log.Fatal(err)
	}
	if config.Log == "syslog" {
		logwriter, err := syslog.New(syslog.LOG_NOTICE, "agitator")
		if err == nil {
			log.SetOutput(logwriter)
		}
	}
	climit = config.Conlim
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
	table := make(RouteTable, len(conf.Route))
	for path, app := range conf.Route {
		if len(app.Hosts) == 0 {
			log.Println("No routes for", path)
			continue
		}
		p := Path{}
		switch app.Mode {
		case "", "failover":
			p.Mode = "failover"
		case "balance":
			p.Mode = "balance"
		default:
			log.Println("Invalid mode for", path)
			continue
		}
		p.Hosts = make(map[string]*SrvPref, len(app.Hosts))
		for i, host := range app.Hosts {
			if strings.Index(host, ":") == -1 || strings.Index(host, "]") == len(host)-1 {
				// Add default port to host string if not present
				host += agiPort
			}
			s := SrvPref{}
			s.Priority = i
			s.Count = 0
			p.Hosts[host] = &s
		}
		table[path] = &p
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
	updateCount(strings.TrimPrefix(sess.Request.Path, "/"), sess.Request.Host, -1)
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
	hosts := make([]Server, 0, len(dest.Hosts))
	for srv, pref := range dest.Hosts {
		pref.RLock()
		if climit > 0 && pref.Count >= climit {
			if debug {
				log.Println("Reached the connections limit in", srv)
			}
			pref.RUnlock()
			continue
		}
		hosts = append(hosts, Server{srv, pref.Priority, pref.Count})
		pref.RUnlock()
	}
	if len(hosts) == 0 {
		return fmt.Errorf("No routes available")
	}
	if dest.Mode == "balance" {
		// Load Balance mode: Sort server by number of active sessions
		sort.Sort(PerActive(hosts))
	} else {
		// Failover mode: Sort server list by priority
		sort.Sort(PerPriority(hosts))
	}
	for _, server := range hosts {
		tcpaddr, err = net.ResolveTCPAddr("tcp", server.Host)
		if err != nil {
			continue
		}
		s.ServerCon, err = net.DialTCP("tcp", nil, tcpaddr)
		if err == nil {
			s.Request.Host = server.Host
			updateCount(path, server.Host, 1)
			break
		} else if debug {
			log.Println("Failed to connect to", tcpaddr, err)
		}
	}
	return err
}

// Update active session counter
func updateCount(path, host string, i int) {
	dest, ok := rtable[path]
	if !ok {
		dest, ok = rtable["*"]
		if !ok {
			return
		}
	}
	if _, ok = dest.Hosts[host]; ok {
		dest.Hosts[host].Lock()
		dest.Hosts[host].Count += i
		dest.Hosts[host].Unlock()
	}
}
