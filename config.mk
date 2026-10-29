# arcus version
VERSION = 0.6.1

# paths
PREFIX = /usr/local

# flags
GOFLAGS = -trimpath
# -s -w: strip symbol table and DWARF; -X: inject version string
LDFLAGS = -s -w -X main.Version=${VERSION}

# compiler
GO = go
