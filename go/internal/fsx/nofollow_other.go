//go:build !unix

package fsx

// oNoFollow is zero where the platform has no O_NOFOLLOW, leaving [OpenAppend]
// with only its Lstat, which can be raced. brb runs on Linux; this exists only
// so the package still compiles elsewhere.
const oNoFollow = 0
