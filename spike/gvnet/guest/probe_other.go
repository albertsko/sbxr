//go:build !linux

package main

import "fmt"

func probe(string, []string) { fmt.Println("probe needs linux") }
