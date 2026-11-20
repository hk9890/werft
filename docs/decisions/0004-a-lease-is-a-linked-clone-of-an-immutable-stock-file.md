# 0004: A lease is a linked clone: a thin qcow2 overlay on a flat, immutable stock file

A lease must exist in milliseconds, and the stock file must never change, so that many leases share
one stock version.
