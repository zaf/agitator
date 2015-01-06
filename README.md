#agitator

A reverse proxy for the FastAGI protocol

!!Work in progress - Not stable yet!!

Features:

* Request based routing
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

See sample config file for configuration details.

---

Copyright (C) 2014 - 2015, Lefteris Zafiris <zaf.000@gmail.com>

This program is free software, distributed under the terms of
the GNU General Public License Version 3. See the LICENSE file
at the top of the source tree.
