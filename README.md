#agitator

A reverse proxy for the FastAGI protocol

**Work in progress!**

*Features:*

* Request based routing
* TLS support
* Load balancing
* HA - Failover
* On the fly config reloading
* Syslog integration

To install:
```
	go get github.com/zaf/agitator
```
To run:
```
	agitator -conf=/path/to/conf.file
```
*Topology examples:*

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
