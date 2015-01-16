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
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"log/syslog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path"
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
	agiFail    = "FAILURE\n"
)

var (
	confFile    = flag.String("conf", "/usr/local/etc/agitator.conf", "Configuration file")
	rtable      RouteTable
	dialTimeout time.Duration
	climit      int
	debug       bool
	skipVerify  bool
)

// AgiSession holds the data of an active AGI session
type AgiSession struct {
	ClientCon net.Conn
	ServerCon net.Conn
	Request   *url.URL // Client Request
	Server    *Server  // Destination server
}

// Config struct holds the various settings values after parsing the config file.
type Config struct {
	Listen    string
	Port      int
	TLS       bool
	TLSStrict bool   `toml:"tls_strict"`
	TLSCert   string `toml:"tls_cert"`
	TLSKey    string `toml:"tls_key"`
	TLSListen string `toml:"tls_listen"`
	TLSPort   int    `toml:"tls_port"`
	Timeout   int
	Conlim    int
	Log       string
	Debug     bool
	Route     []struct {
		Path string
		Mode string
		Host []struct {
			Addr string
			Port string
			TLS  bool
		}
	}
}

// RouteTable holds the routing table
type RouteTable struct {
	sync.RWMutex
	Route map[string]*Destination
}

// Destination struct holds a list of hosts and the routing mode
type Destination struct {
	sync.RWMutex
	Hosts []*Server
	Mode  string
}

// Server struct holds the server address, TLS setting and the number of active sessions
type Server struct {
	sync.RWMutex
	Host  string
	TLS   bool
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
	log.SetFlags(0)
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
	skipVerify = !config.TLSStrict

	// Generate routing table from config file data
	table, err := genRtable(config)
	if err != nil {
		log.Fatal(err)
	}
	rtable.Lock()
	rtable.Route = table
	rtable.Unlock()

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

	// Create a TLS listener
	if config.TLS {
		cert, err := tls.LoadX509KeyPair(config.TLSCert, config.TLSKey)
		if err != nil {
			log.Fatal(err)
		}
		tlsConf := tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS10}
		tlsSrv := config.TLSListen + ":" + strconv.Itoa(config.TLSPort)
		log.Printf("Listening for TLS connections on %v\n", tlsSrv)
		tlsLn, err := tls.Listen("tcp", tlsSrv, &tlsConf)
		if err != nil {
			log.Fatal(err)
		}
		defer tlsLn.Close()

		go func() {
			for atomic.LoadInt32(&shutdown) == 0 {
				conn, err := tlsLn.Accept()
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
	}

	config = Config{}
	wg.Wait()
	return
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
		sess.ClientCon.Write([]byte(agiFail))
		return
	}
	// Do the routing
	err = sess.route()
	if err != nil {
		log.Println(err)
		sess.ClientCon.Write([]byte(agiFail))
		return
	}
	defer sess.ServerCon.Close()
	sess.Server.updateCount(1)

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
	sess.Server.updateCount(-1)
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
	reqPath := strings.TrimPrefix(s.Request.Path, "/")

	if debug {
		log.Printf("%v: New request: %s\n", client, s.Request)
	}
	// Find route
	rtable.RLock()
	defer rtable.RUnlock()
	dest, ok := rtable.Route[reqPath]
	for !ok && reqPath != "" {
		reqPath, _ = path.Split(reqPath)
		reqPath = strings.TrimSuffix(reqPath, "/")
		dest, ok = rtable.Route[reqPath]
	}
	if !ok {
		dest, ok = rtable.Route[wildCard]
		if !ok {
			return fmt.Errorf("%v: No route found for %s", client, reqPath)
		}
		if debug {
			log.Printf("%v: Using wildcard route\n", client)
		}
	} else if debug {
		log.Printf("%v: Using route: %s\n", client, reqPath)
	}
	// Load Balance mode: Sort servers by number of active sessions
	if dest.Mode == "balance" {
		dest.Lock()
		sort.Sort(ByActive(dest.Hosts))
		dest.Unlock()
	}

	// Find available servers and connect
	for i := 0; i < len(dest.Hosts); i++ {
		server := dest.Hosts[i]
		server.RLock()
		if climit > 0 && server.Count >= climit {
			server.RUnlock()
			log.Printf("%v: Reached connections limit in %s\n", client, server.Host)
			continue
		}
		server.RUnlock()
		dialer := new(net.Dialer)
		dialer.Timeout = dialTimeout
		if server.TLS {
			tslConf := tls.Config{InsecureSkipVerify: skipVerify}
			s.ServerCon, err = tls.DialWithDialer(dialer, "tcp", server.Host, &tslConf)
		} else {
			s.ServerCon, err = dialer.Dial("tcp", server.Host)
		}
		if err == nil {
			s.Request.Host = server.Host
			s.Server = server
			return err
		} else if debug {
			log.Printf("%v: Failed to connect to %s, %s\n", client, server.Host, err)
		}
	}

	//No servers found
	return fmt.Errorf("%v: Unable to connect to any server", client)
}

// Update active session counter
func (s *Server) updateCount(i int) {
	s.Lock()
	s.Count += i
	s.Unlock()
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
			log.Printf("Received %v, reloading routing rules from config file %s\n", signal, *confFile)
			var config Config
			_, err := toml.DecodeFile(*confFile, &config)
			if err != nil {
				log.Println("Failed to read config file:", err)
				break
			}
			// Generate routing table from config file data
			table, err := genRtable(config)
			if err != nil {
				log.Println("No routes specified, using old config data.")
				break
			}
			rtable.Lock()
			rtable.Route = table
			rtable.Unlock()
		}
	}
}

// Generate Routing table from config data
func genRtable(conf Config) (map[string]*Destination, error) {
	var err error
	table := make(map[string]*Destination, len(conf.Route))
	for _, route := range conf.Route {
		if len(route.Host) == 0 {
			log.Println("No routes for", route.Path)
			continue
		}
		p := new(Destination)
		switch route.Mode {
		case "", "failover":
			p.Mode = "failover"
		case "balance":
			p.Mode = "balance"
		default:
			log.Println("Invalid mode for", route.Path)
			continue
		}
		p.Hosts = make([]*Server, 0, len(route.Host))
		for _, server := range route.Host {
			if server.Port == "" {
				server.Port = agiPort
			}
			s := new(Server)
			s.Host = server.Addr + ":" + server.Port
			s.TLS = server.TLS
			p.Hosts = append(p.Hosts, s)
		}
		table[route.Path] = p
		if debug {
			log.Printf("Added %s route\n", route.Path)
		}
	}
	if len(table) == 0 {
		err = fmt.Errorf("No routes specified")
	}
	return table, err
}
