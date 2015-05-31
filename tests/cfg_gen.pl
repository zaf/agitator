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

my $general = "listen = \"0.0.0.0\"\nport = 4573\ntls_strict = true\ntls = true\ntls_listen = \"0.0.0.0\"\ntls_port = 4574\n"
		. "tls_cert = \"tests/public.crt\"\ntls_key  = \"tests/secret.key\"\nfwd_for = false\ntimeout = 3\nlog = \"stdout\""
		. "\ndebug = false\nthreads = 1\n";
my $host = "\n\t\[\[route.host\]\]\n\taddr = \"localhost\"\n\tport= 4545\n\ttls = false\n\tmax = 0\n";

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
		print "mode = \"balance\"\n" if ($_ % 2 != 0);
		print $host x $num_hosts;
	}
}
