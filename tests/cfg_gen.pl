#!/usr/bin/env perl

#
# Agitator testing config generator
#

use strict;
use warnings;

my $paths = 1024;
my $hosts = 1024;
my @chars = ("A".."Z", "a".."z");

my $host_list = "\"localhost:4546\", \"127.0.0.1:4545\", ";
my $general = "listen = \"0.0.0.0\"\nport = 4573\ntimeout = 3\nconlim = 16384\nlog = \"stdout\"\ndebug = false\n\n";
my $wildcard = "\[route\.\*\]\nmode = \"balance\"\nhosts = \[ ". $host_list x $hosts . " \]\n\n";

print($general, $wildcard);

for (1..$paths) {
	my $path;
	$path .= $chars[rand @chars] for 1..9;
	print("\[route.". $path . "\]\n");
	print("mode = \"balance\"\n") if ($_ % 2 == 0);
	print("hosts = \[ ". $host_list x $hosts . "\]\n\n");
}
