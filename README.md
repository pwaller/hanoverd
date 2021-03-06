# This project is in the early prototype / dreaming phase

Hanoverd - The docker handover daemon
-------------------------------------

Hanoverd ("Hanover-Dee") is responsible for managing seamless transitions from
one application version to another with Docker containers.

## Installation

Hanoverd currently requires the ability to mess with iptables. This can be
achieved with setcap to avoid using root.

A suitably capable `iptables` command must be on the `$PATH`. This can be
achieved with this command:

```
cp $(which iptables) .
sudo setcap 'cap_net_admin,cap_net_raw=+ep' iptables
PATH=.:$PATH hanoverd
```

(or you can use some directory other than `.`).

## User experience

* You run one hanoverd per application you wish to run in Docker.
* You run hanoverd in a directory containing a `Dockerfile`.
* Starting hanoverd causes that `Dockerfile` to be built and the application is
  started.
* Sending hanoverd a signal results in hanoverd rebuilding the container and
  transitioning to the new version.
* When the new version successfully accepts a connection, the new version
  becomes live.
* If the new version fails to accept any connection, the old version will chug
  along happily until a new request comes in to start a new application version.

## Building and running

First build it:

```
$ go get -v
$ go build -v
```

Then launch it (in a directory with a Dockerfile!):

```
$ ./hanoverd
```

Environment variables which the docker client (and boot2docker) use
can be set first.

  DOCKER\_CERT\_PATH
  DOCKER\_HOST
  DOCKER\_TLS\_VERIFY

## Method

* Hanoverd is responsible for listening on externally visible sockets and
  forwards them to the currently active docker application.
* Hanoverd can be signalled via POSIX signals or via a HTTP ping.

## Plans

* Hanoverd is not currently responsible for receiving a hook to run updates -
  that may be the responsibility of another application
* Hanoverd can provide log messages live via a web interface.
* "Power on self test" allows hanoverd to also run some tests before declaring
  the new container ready to go live.
