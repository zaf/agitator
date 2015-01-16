#agitator

A reverse proxy for the [FastAGI](https://wiki.asterisk.org/wiki/display/AST/AGI+Commands) protocol

**Features:**

- *Request based routing*

 Routes FastAGI sessions depending on the request URI.

- *Load balancing*

 Distributes load evenly between FastAGI servers based on the number of active sessions.

- *HA - Failover*

 Routes traffic to stand-by servers when main server is not reachable, or has reached the number of maximum allowed sessions.

- *TLS*

 Offers strong encryption support for FastAGI sessions.

- *On the fly config reloading*

 Routing rules can be reloaded/altered on the fly without restarting the proxy or dropping any connections.

- *Syslog integration*

 Can send logging and debug data to a syslog server.


To install:
```
	go get github.com/zaf/agitator
```
To run:
```
	agitator -conf=/path/to/conf.file
```

There is also a simple Makefile that installs the agitator binary in `/usr/local/bin` and its config file in
`/usr/local/etc` to help with system wide installation. Under the folder [init](https://github.com/zaf/agitator/tree/master/init)
startup scripts are provided for systemd based systems or for Red Hat and Debian based systems that use the old sysV init.

Reloading the config file on-the-fly requires a Hangup signal to be sent to agigator process. This can be done either by using the
init scripts provided or with something like `pkill -HUP agitator`. This reloads the routing settings from the config file,
to alter the general settings (like listening port or address) agitator must be restarted.
A graceful shutdown option is available where agitator stops accepting new requests but waits
for all active sessions to finish before exiting. To trigger it again either use the init scripts
or send an Interrupt signal (`pkill -INT agitator`) to the agitator process.

**Topology examples:**

A typical layout where agitator sits between one or more asterisk servers and one or more
FastAGI servers routing AGI requests providing load balancing and/or high availability:

![alt text](https://raw.githubusercontent.com/zaf/agitator/master/doc/example-1.png)

Agitator can make up for asterisk's lack of encryption support on the AGI protocol
either by wrapping AGI sessions in TLS, sending the traffic over the network,
unwrapping it and contacting the FastAGI server:

![alt text](https://raw.githubusercontent.com/zaf/agitator/master/doc/example-3.png)

or by encrypting AGI traffic and directly communicating with TLS aware FastAGI servers:

![alt text](https://raw.githubusercontent.com/zaf/agitator/master/doc/example-2.png)

An example of such a FastAGI server can be found [here](https://github.com/zaf/agi/blob/master/examples/fastagi-tls.go),
using the [Go AGI package](https://github.com/zaf/agi).

See sample [config file](https://github.com/zaf/agitator/blob/master/sample.conf) for configuration details.

---

Copyright (C) 2014 - 2015, Lefteris Zafiris <zaf.000@gmail.com>

This program is free software, distributed under the terms of
the GNU General Public License Version 3. See the LICENSE file
at the top of the source tree.
