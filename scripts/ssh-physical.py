#!/usr/bin/env python3
"""macOS SSH ProxyCommand: bind a physical interface without changing TUN routes."""
import os
import select
import socket
import sys

host, port, interface = sys.argv[1:]
connection = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
connection.setsockopt(socket.IPPROTO_IP, 25, socket.if_nametoindex(interface))  # IP_BOUND_IF
connection.settimeout(15)
connection.connect((host, int(port)))
connection.settimeout(None)
inputs = [connection, 0]
while inputs:
    ready, _, _ = select.select(inputs, [], [])
    for source in ready:
        if source == 0:
            data = os.read(0, 65536)
            if data:
                connection.sendall(data)
            else:
                connection.shutdown(socket.SHUT_WR)
                inputs.remove(0)
        else:
            data = connection.recv(65536)
            if not data:
                connection.close()
                sys.exit(0)
            while data:
                data = data[os.write(1, data):]
