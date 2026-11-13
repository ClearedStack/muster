package httpapi

// What the tests need to see of the server and cannot reach from
// outside the package.

// Paths lists every call the server answers.
func Paths() []string {
	out := make([]string, 0, len(calls))
	for path := range calls {
		out = append(out, path)
	}
	return out
}

// Occupy takes every place there is for a call in flight, as a rush of
// callers would, and returns a function that gives them back.
func (s *Server) Occupy() (release func()) {
	for i := 0; i < cap(s.busy); i++ {
		s.busy <- struct{}{}
	}
	return func() {
		for i := 0; i < cap(s.busy); i++ {
			<-s.busy
		}
	}
}
