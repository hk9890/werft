package lease

// SetNewID makes every new lease ID id until the test ends with restore.
func SetNewID(id string) (restore func()) {
	old := newID
	newID = func() string { return id }
	return func() { newID = old }
}
