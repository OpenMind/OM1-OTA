package version

// BuildTimestamp is a decimal Unix-seconds string, overwritten at build time
// via `-ldflags -X`. It defaults to "0" for local/dev builds so any real
// release always compares as newer.
var BuildTimestamp = "0"
