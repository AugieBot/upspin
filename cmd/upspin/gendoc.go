// Copyright 2017 The Upspin Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build gendoc

package main

import (
	"bytes"
	"fmt"
	"go/format"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"sort"
	"strings"

	"upspin.io/flags"
)

func init() {
	commands["gendoc"] = (*State).gendoc
}

const docHeader = `// Copyright 2017 The Upspin Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Code generated by upspin gendoc. DO NOT EDIT.
// After editing a command's usage, run 'go generate' to update this file.

/*
`

var flagDocs []string

func (s *State) gendoc(args ...string) {
	var names []string
	for name := range commands {
		if name == "gendoc" {
			continue
		}
		names = append(names, name)
	}
	names = append(names, externalCommands...)
	// Shell is not in "commands" to prevent init loops.
	names = append(names, "shell")
	sort.Strings(names)

	var b bytes.Buffer

	fmt.Fprintln(&b, docHeader)

	// Generate package doc followed by flags.
	upspin := os.Args[0]
	s.helpDocs(&b, upspin)

	// Generate the flag output and also remember it for filtering subcommand help.
	var tmp bytes.Buffer
	s.helpDocs(&tmp, upspin, "-help")
	b.Write(tmp.Bytes())
	flagDocs = toLines(tmp.String())

	// Generate subcommands.
	for _, name := range names {
		if name != "shell" {
			// Make sure command exists; this will error and exit if not.
			s.getCommand(name)
		}
		var docs bytes.Buffer
		s.helpDocs(&docs, upspin, name, "-help")

		fmt.Fprintf(&b, "\n\nSub-command %s\n\n", name)
		fmt.Fprintf(&b, "%s\n", docs.Bytes())
	}
	fmt.Fprintln(&b, "*/\npackage main")

	out, err := format.Source(b.Bytes())
	if err != nil {
		s.Exit(err)
	}
	err = ioutil.WriteFile("doc.go", out, 0644)
	if err != nil {
		s.Exit(err)
	}
}

func (s *State) helpDocs(out io.Writer, command string, args ...string) {
	cmd := exec.Command(command, append(flags.Args(), args...)...)
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &b
	cmd.Env = append(os.Environ(), "UPSPIN_GENDOC=yes")
	cmd.Run()
	// Command should have exited with status 2, which we ignore,
	// but if it's an external command the output may end with a line
	// announcing that; remove it. Also remove any information about
	// the global flags by comparing the text to the output of
	// "upspin -help", saved above.
	// This method isn't the cheapest, but it's easy.
	home := os.Getenv("HOME")
	lines := toLines(b.String())
Without:
	for _, line := range lines {
		// TODO: This is ugly and Unix-specific. Is there a better way?
		if strings.HasPrefix(line, "upspin: ") && strings.HasSuffix(line, ": exit status 2") {
			continue Without
		}
		for _, flagLine := range flagDocs {
			if line == flagLine {
				continue Without
			}
		}
		line = strings.Replace(line, home, "/home/user", -1)
		fmt.Fprintf(out, "%s\n", line)
	}
}

func toLines(data string) []string {
	lines := strings.Split(data, "\n")
	// Last line will be an empty slice after the final newline; delete it.
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		return lines[:len(lines)-1]
	}
	return lines
}
