Godeps - a simple dependency locking tool for Go

The godeps command can calculate the current set of dependencies of a set of Go packages, and knows how to update dependencies to a previously calculated set, including fetching new versions when necessary.

It is careful to avoid overwriting dependencies that have uncommitted changes.

Usage:
	godeps [flags] [pkg ...]
	godeps -u file [flags]

In the first form of usage (without the -u flag), godeps prints to
standard output a list of all the source dependencies of the named
packages (or the package in the current directory if none is given).
If there is ambiguity in the source-control systems used, godeps will
print all the available versions and an error, exiting with a false
status. It is up to the user to remove lines from the output to make
the output suitable for input to godeps -u.

In the second form, godeps updates source to versions specified by
the -u file argument, which should hold version information in the
same form printed by godeps. It is an error if the file contains more
than one line for the same package root. If a specified revision is not
currently available, godeps will attempt to fetch it, unless the -F flag
is provided.

     -F	when updating, do not try to fetch deps if the update fails
     -N	when updating, only update if the dependency is newer
     -P int
       	max number of concurrent updates (default 1)
     -T	do not include testing dependencies
     -force-clean
       	force cleaning of modified-but-not-committed repositories. Do not use this flag unless you really need to!
     -n	print but do not execute update commands
     -u string
       	update dependencies
     -x	show executed commands
