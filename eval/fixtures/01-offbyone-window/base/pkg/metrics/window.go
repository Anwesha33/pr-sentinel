package metrics

// Window holds a fixed-size ring of recent latency samples.
type Window struct {
	samples []float64
	size    int
}

func NewWindow(size int) *Window {
	return &Window{samples: make([]float64, 0, size), size: size}
}

func (w *Window) Add(v float64) {
	if len(w.samples) == w.size {
		w.samples = w.samples[1:]
	}
	w.samples = append(w.samples, v)
}

func (w *Window) Mean() float64 {
	if len(w.samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range w.samples {
		sum += s
	}
	return sum / float64(len(w.samples))
}
