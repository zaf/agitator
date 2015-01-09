#!/usr/bin/env perl

#
# Agitator testing config generator
#

use strict;
use warnings;

if (!$ARGV[0] or $ARGV[0] eq '-h' or $ARGV[0] eq '--help') {
	print "Agitator testing config generator.\n\nUsage: $0 [NUMBER OF ROUTES] [NUMBER OF HOSTS PER ROUTE]\n";
	exit 1;
}

my $paths = $ARGV[0];
my $hosts = $ARGV[1];
my @chars = ("A".."Z", "a".."z", "/");

my $general = "listen = \"0.0.0.0\"\nport = 4573\ntimeout = 3\nconlim = 16384\nlog = \"stdout\"\ndebug = false\n\n";
my $host_list = "\"localhost:4546\", \"127.0.0.1:4545\", ";
my $wildcard = "\[route\.\*\]\nmode = \"balance\"\nhosts = \[ ". $host_list x $hosts . " \]\n\n";

print $general;

if ($paths) {
	print $wildcard;
	for (2..$paths) {
		my $path;
		$path .= $chars[rand @chars] for 0..9;
		print("\[route.". $path . "\]\n");
		print("mode = \"balance\"\n") if ($_ % 2 != 0);
		print("hosts = \[ ". $host_list x $hosts . "\]\n\n");
	}
}
