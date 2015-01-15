#agitator

A reverse proxy for the FastAGI protocol

!! Work in progress !!

Features:

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
Topology examples:

![alt text](https://raw.githubusercontent.com/zaf/agitator/master/doc/example-1.png)

![alt text](https://raw.githubusercontent.com/zaf/agitator/master/doc/example-3.png)

![alt text](https://raw.githubusercontent.com/zaf/agitator/master/doc/example-2.png)

See sample [config file](https://github.com/zaf/agitator/blob/master/sample.conf) for configuration details.

---

Copyright (C) 2014 - 2015, Lefteris Zafiris <zaf.000@gmail.com>

This program is free software, distributed under the terms of
the GNU General Public License Version 3. See the LICENSE file
at the top of the source tree.
