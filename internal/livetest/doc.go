// Package livetest holds the helpers of the live tests, which run against
// qemu:///system behind build tag live (docs/TESTING.md). Every domain it
// creates has a name that starts with "werft-test-", and every volume lives
// in pool werft-test.
package livetest
