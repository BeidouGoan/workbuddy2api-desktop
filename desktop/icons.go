package main

import _ "embed"

//go:embed assets/app.ico
var iconICO []byte

func iconBytes() []byte { return iconICO }
