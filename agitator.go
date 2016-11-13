/*
	A FastAGI reverse proxy

	Copyright (C) 2014 - 2015, Lefteris Zafiris <zaf@fastmail.com>

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
	"log"
	"log/syslog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path"
	"runtime"
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
	agiEnvMax  = 151
	agiEnvSize = 512
	wildCard   = "*"
	agiPort    = 4573
	agiFail    = "FAILURE\n"
	balance    = "balance"
	failover   = "failover"
	roundRobin = "round-robin"
)

var (
	confFile    = flag.String("conf", "/usr/local/etc/agitator.conf", "Configuration file")
	config      Config
	rtable      RouteTable
	addFwdFor   bool
	dialTimeout time.Duration
	srvTimeout  time.Duration
	cltTimeout  time.Duration
	debug       bool
	clientTLS   tls.Config
)

// AgiSession holds the data of an active AGI session
type AgiSession struct {
	ClientCon net.Conn
	ServerCon net.Conn
	FwdFor    string   // List of originating IPs
	Request   *url.URL // Client Request
	Server    *Server  // Destination server
}

// Config struct holds the various settings values after parsing the config file.
type Config struct {
	Listen     string
	Port       uint16
	TLSListen  string `toml:"tls_listen"`
	TLSPort    uint16 `toml:"tls_port"`
	TLSStrict  bool   `toml:"tls_strict"`
	TLSCert    string `toml:"tls_cert"`
	TLSKey     string `toml:"tls_key"`
	FwdFor     bool   `toml:"fwd_for"`
	ConTimeout int    `toml:"con_timeout"`
	SrvTimeout int    `toml:"srv_timeout"`
	CltTimeout int    `toml:"clt_timeout"`
	Log        string
	Debug      bool
	Threads    int
	Route      []struct {
		Path string
		Mode string
		Host []struct {
			Addr string
			Port uint16
			TLS  bool
			Max  int
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
	Max   int
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

func init() {
	flag.Parse()
	// Parse Config file
	_, err := toml.DecodeFile(*confFile, &config)
	if err != nil {
		log.Fatal(err)
	}
	runtime.GOMAXPROCS(config.Threads)
	// Setup logging
	if config.Log == "syslog" {
		logwriter, err := syslog.New(syslog.LOG_NOTICE|syslog.LOG_USER, "agitator")
		if err == nil {
			log.SetFlags(0)
			log.SetOutput(logwriter)
		}
	}
	if config.Listen == "" && config.TLSListen == "" {
		log.Fatal("No listening address specified.")
	}
	// Set some settings as global vars
	addFwdFor = config.FwdFor
	dialTimeout = time.Duration(float64(config.ConTimeout)) * time.Second
	srvTimeout = time.Duration(float64(config.SrvTimeout)) * time.Second
	cltTimeout = time.Duration(float64(config.CltTimeout)) * time.Second
	clientTLS = tls.Config{InsecureSkipVerify: !config.TLSStrict}
	debug = config.Debug

	// Generate routing table from config file data
	table, err := genRtable(config)
	if err != nil {
		log.Fatal(err)
	}
	rtable.Lock()
	rtable.Route = table
	rtable.Unlock()
}

func main() {
	wg := new(sync.WaitGroup)
	// Handle signals
	var shutdown int32
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGHUP)
	wg.Add(1)
	go sigHandle(sigChan, &shutdown, wg)

	if config.Listen != "" {
		// Create a listener and start a new goroutine for each connection.
		addr := config.Listen + ":" + strconv.Itoa(int(config.Port))
		log.Printf("Starting AGItator proxy on %v\n", addr)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatal(err)
		}
		defer ln.Close()
		go serve(ln, wg, &shutdown)
	}

	if config.TLSListen != "" {
		// Create a TLS listener
		cert, err := tls.LoadX509KeyPair(config.TLSCert, config.TLSKey)
		if err != nil {
			log.Fatal(err)
		}
		tlsConf := tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS11}
		tlsSrv := config.TLSListen + ":" + strconv.Itoa(int(config.TLSPort))
		log.Printf("Listening for TLS connections on %v\n", tlsSrv)
		tlsLn, err := tls.Listen("tcp", tlsSrv, &tlsConf)
		if err != nil {
			log.Fatal(err)
		}
		defer tlsLn.Close()
		go serve(tlsLn, wg, &shutdown)
	}

	config = Config{}
	wg.Wait()
	return
}

// Accept incoming connections
func serve(ln net.Listener, wg *sync.WaitGroup, shutdown *int32) {
	for atomic.LoadInt32(shutdown) == 0 {
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
}

// Connection handler. Find route, connect to remote server and relay data.
func connHandle(conn net.Conn, wg *sync.WaitGroup) {
	sess := new(AgiSession)
	sess.ClientCon = conn
	if cltTimeout.Seconds() > 0.0 {
		sess.ClientCon.SetReadDeadline(time.Now().Add(cltTimeout))
	}
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
	if addFwdFor {
		if sess.FwdFor == "" {
			sess.FwdFor = sess.ClientCon.RemoteAddr().String()
		} else {
			sess.FwdFor += ", " + sess.ClientCon.RemoteAddr().String()
		}
		env = append(env, []byte("agi_x_fwd_for: "+sess.FwdFor+"\n")...)
	}
	env = append(env, []byte("agi_request: "+sess.Request.String()+"\n\n")...)
	_, err = sess.ServerCon.Write(env)
	if err != nil {
		log.Println(err)
		sess.ClientCon.Write([]byte(agiFail))
		sess.Server.updateCount(-1)
		return
	}

	// Relay data between the 2 connections.
	done := make(chan int)
	go func() {
		conCopy(sess.ServerCon, sess.ClientCon, cltTimeout)
		done <- client
	}()
	go func() {
		conCopy(sess.ClientCon, sess.ServerCon, srvTimeout)
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
	var line []byte
	agiEnv := make([]byte, 0, agiEnvSize)
	buf := bufio.NewReader(s.ClientCon)

	// Read the AGI environment, store all vars in agiEnv except 'agi_request'.
	// Request is stored separately for parsing and further processing.
	for i := 0; i <= agiEnvMax; i++ {
		line, err = buf.ReadBytes(10)
		if err != nil || len(line) <= len("\r\n") {
			break
		}
		ind := bytes.IndexByte(line, ':')
		if ind == -1 {
			break
		}
		if string(line[:ind]) == "agi_request" && len(line) >= ind+len(": \n") {
			ind += len(": ")
			req = string(line[ind : len(line)-1])
		} else if addFwdFor && string(line[:ind]) == "agi_x_fwd_for" && len(line) >= ind+len(": \n") {
			ind += len(": ")
			s.FwdFor = string(line[ind : len(line)-1])
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

// Route based on request path
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
	if dest.Mode == balance && len(dest.Hosts) > 1 {
		dest.Lock()
		sort.Sort(ByActive(dest.Hosts))
		dest.Unlock()
	}
	// Round Robin mode: Cycle through the servers list
	if dest.Mode == roundRobin && len(dest.Hosts) > 1 {
		dest.Lock()
		dest.Hosts = append(dest.Hosts[1:], dest.Hosts[0])
		dest.Unlock()
	}
	// Find available servers and connect
	for i := 0; i < len(dest.Hosts); i++ {
		server := dest.Hosts[i]
		server.RLock()
		if server.Max > 0 && server.Count >= server.Max {
			server.RUnlock()
			log.Printf("%v: Reached connections limit in %s\n", client, server.Host)
			continue
		}
		server.RUnlock()
		dialer := new(net.Dialer)
		dialer.Timeout = dialTimeout
		if server.TLS {
			s.ServerCon, err = tls.DialWithDialer(dialer, "tcp", server.Host, &clientTLS)
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
			var conf Config
			_, err := toml.DecodeFile(*confFile, &conf)
			if err != nil {
				log.Println("Failed to read config file:", err)
				continue
			}
			// Generate routing table from config file data
			table, err := genRtable(conf)
			conf = Config{}
			if err != nil {
				log.Println("No routes specified, using old config data.")
				continue
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
		case "", failover:
			p.Mode = failover
		case balance:
			p.Mode = balance
		case roundRobin:
			p.Mode = roundRobin
		default:
			log.Println("Invalid mode for", route.Path)
			continue
		}
		p.Hosts = make([]*Server, 0, len(route.Host))
		for _, server := range route.Host {
			if server.Port < 1 {
				server.Port = agiPort
			}
			s := new(Server)
			s.Host = server.Addr + ":" + strconv.Itoa(int(server.Port))
			s.TLS = server.TLS
			s.Max = server.Max
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

// conCopy copies from src to dst until either EOF is reached on src or an error occurs.
// Similar to io.Copy but also updates the src connection read timeout.
func conCopy(dst, src net.Conn, timeout time.Duration) error {
	var err error
	buf := make([]byte, 1024)
	for {
		if timeout.Seconds() > 0.0 {
			src.SetReadDeadline(time.Now().Add(timeout))
		}
		nr, readErr := src.Read(buf)
		if nr > 0 {
			nw, writeErr := dst.Write(buf[0:nr])
			if writeErr != nil {
				err = writeErr
				break
			}
			if nr != nw {
				err = fmt.Errorf("Short write")
				break
			}
		}
		if readErr != nil {
			if readErr.Error() != "EOF" {
				err = readErr
			}
			break
		}
	}
	return err
}
