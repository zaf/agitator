#!/usr/bin/env perl

#
# Agitator testing config generator
#

use strict;
use warnings;

if (!@ARGV or $ARGV[0] eq '-h' or $ARGV[0] eq '--help') {
	print "Agitator testing config generator.\n\nUsage: $0 [NUMBER OF ROUTES] [NUMBER OF HOSTS PER ROUTE]\n";
	exit 1;
}

my $num_paths = $ARGV[0];
my $num_hosts = $ARGV[1];
my @chars = ("A".."Z", "a".."z", "/");

my $general =
"listen = \"0.0.0.0\"
port = 4573
tls_listen = \"0.0.0.0\"
tls_strict = true
tls_port = 4574
tls_cert = \"tests/public.crt\"
tls_key  = \"tests/secret.key\"
fwd_for = false
timeout = 3
log = \"stderr\"
debug = false\nthreads = 0
";

my $host = "
	\[\[route.host\]\]
	addr = \"localhost\"
	port= 4545
	tls = false
	max = 0
";

print $general;

if ($num_paths) {
	for (1..$num_paths) {
		my $path;
		if ($_ == 1) {
			$path = "*";
		} else {
			$path .= $chars[rand @chars] for 0..9;
		}
		print "\n\[\[route\]\]\npath = \"$path\"\n";
		if ($_ % 3 == 0) {
			print "mode = \"balance\"\n";
		} elsif ($_ % 2 == 0) {
			print "mode = \"round-robin\"\n";
		}
		print $host x $num_hosts;
	}
}
