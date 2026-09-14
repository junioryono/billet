package guestassets

// WriteExecutable gives the external test package the one fork-locked write,
// so every executable a test in this directory execs is written the same way.
var WriteExecutable = writeExecutable
