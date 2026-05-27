package main

import "time"

// timeNow is a variable function so tests can mock the clock
var timeNow = time.Now
