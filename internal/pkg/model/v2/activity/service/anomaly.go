package service

import (
	"encoding/json"
	"fmt"
	"math"
	"math/bits"
	"sync"
)

const (
	// CDF16Fixed is the shift for 16 bit coders
	CDF16Fixed = 16 - 3
	// CDF16Scale is the scale for 16 bit coder
	CDF16Scale = 1 << CDF16Fixed
	// CDF16Rate is the damping factor for 16 bit coder
	CDF16Rate = 5
	// CDF16Size is the size of the cdf
	CDF16Size = 256
	// CDF16Depth is the depth of the context tree
	CDF16Depth = 2
)

// Node16 is a context node
type Node16 struct {
	Model    []uint16
	Children map[uint16]*Node16
}

// NewNode16 creates a new context node
func NewNode16() *Node16 {
	model, children, sum := make([]uint16, CDF16Size+1), make(map[uint16]*Node16), 0
	for i := range model {
		model[i] = uint16(sum)
		sum += 32
	}
	return &Node16{
		Model:    model,
		Children: children,
	}
}

// CDF16 is a context based cumulative distributive function model
// https://fgiesen.wordpress.com/2015/05/26/models-for-adaptive-arithmetic-coding/
type CDF16 struct {
	Root  *Node16
	Mixin [][]uint16
}

// NewCDF16 creates a new CDF16 with a given context depth
func NewCDF16() *CDF16 {
	root, mixin := NewNode16(), make([][]uint16, CDF16Size)

	for i := range mixin {
		sum, m := 0, make([]uint16, CDF16Size+1)
		for j := range m {
			m[j] = uint16(sum)
			sum++
			if j == i {
				sum += CDF16Scale - CDF16Size
			}
		}
		mixin[i] = m
	}

	return &CDF16{
		Root:  root,
		Mixin: mixin,
	}
}

// Context16 is a 16 bit context
type Context16 struct {
	Context []uint16
	First   int
}

// NewContext16 creates a new context
func NewContext16(depth int) *Context16 {
	return &Context16{
		Context: make([]uint16, depth),
	}
}

// ResetContext resets the context
func (c *Context16) ResetContext() {
	c.First = 0
	for i := range c.Context {
		c.Context[i] = 0
	}
}

// AddContext adds a symbol to the context
func (c *Context16) AddContext(s uint16) {
	context, first := c.Context, c.First
	length := len(context)
	if length > 0 {
		context[first], c.First = s, (first+1)%length
	}
}

// Model gets the model for the current context
func (c *CDF16) Model(ctxt *Context16) []uint16 {
	context := ctxt.Context
	length := len(context)
	var lookUp func(n *Node16, current, depth int) *Node16
	lookUp = func(n *Node16, current, depth int) *Node16 {
		if depth >= length {
			return n
		}

		node := n.Children[context[current]]
		if node == nil {
			return n
		}
		child := lookUp(node, (current+1)%length, depth+1)
		if child == nil {
			return n
		}
		return child
	}

	return lookUp(c.Root, ctxt.First, 0).Model
}

// Update updates the model
func (c *CDF16) Update(s uint16, ctxt *Context16) {
	context, first, mixin := ctxt.Context, ctxt.First, c.Mixin[s]
	length := len(context)
	var update func(n *Node16, current, depth int)
	update = func(n *Node16, current, depth int) {
		model := n.Model
		size := len(model) - 1

		for i := 1; i < size; i++ {
			a, b := int(model[i]), int(mixin[i])
			model[i] = uint16(a + ((b - a) >> CDF16Rate))
		}

		if depth >= length {
			return
		}

		node := n.Children[context[current]]
		if node == nil {
			node = NewNode16()
			n.Children[context[current]] = node
		}
		update(node, (current+1)%length, depth+1)
	}

	update(c.Root, first, 0)
	ctxt.AddContext(s)
}

// Complexity is an entorpy based anomaly detector
type Complexity struct {
	*CDF16
	depth          int
	count          int
	mean, dSquared float32
	sync.RWMutex
}

// NewComplexity creates a new entorpy based model
func NewComplexity(depth int) *Complexity {
	return &Complexity{
		CDF16: NewCDF16(),
		depth: depth,
	}
}

// Complexity outputs the complexity
func (c *Complexity) Complexity(input []byte) (float32, int) {
	var total uint64
	ctxt := NewContext16(c.depth)
	c.RLock()
	for _, s := range input {
		model := c.Model(ctxt)
		total += uint64(bits.Len16(model[s+1] - model[s]))
		ctxt.AddContext(uint16(s))
	}
	c.RUnlock()

	ctxt.ResetContext()
	c.Lock()
	for _, s := range input {
		c.Update(uint16(s), ctxt)
	}

	complexity := float32(CDF16Fixed+1) - (float32(total) / float32(len(input)))
	// https://dev.to/nestedsoftware/calculating-standard-deviation-on-streaming-data-253l
	c.count++
	count := c.count
	mean, n := c.mean, float32(count)
	meanDifferential := (complexity - mean) / n
	newMean := mean + meanDifferential
	dSquaredIncrement := (complexity - newMean) * (complexity - mean)
	newDSquared := c.dSquared + dSquaredIncrement
	c.mean, c.dSquared = newMean, newDSquared
	c.Unlock()

	stddev := float32(math.Sqrt(float64(newDSquared / n)))
	normalized := (complexity - newMean) / stddev
	if normalized < 0 {
		normalized = -normalized
	}
	if math.IsNaN(float64(normalized)) {
		normalized = 0
	}

	return normalized, count
}

// Contexts is a set of anomaly contexts
type Contexts struct {
	contexts map[string]*Complexity
	sync.RWMutex
}

// Lookup looks up a complexity model
func (c *Contexts) Lookup(context string, depth int) *Complexity {
	context = fmt.Sprintf("%s.%d", context, depth)

	c.RLock()
	complexity := c.contexts[context]
	c.RUnlock()
	if complexity != nil {
		return complexity
	}
	complexity = NewComplexity(depth)
	c.Lock()
	c.contexts[context] = complexity
	c.Unlock()
	return complexity
}

var contexts = Contexts{
	contexts: make(map[string]*Complexity),
}

// Anomaly is an anomaly detector
type Anomaly struct {
	values     map[string]interface{}
	context    string
	depth      int
	Complexity float32 `json:"complexity"`
	Count      int     `json:"count"`
}

// InitializeAnomaly creates an anomaly detection service
func InitializeAnomaly(settings map[string]interface{}) (service *Anomaly, err error) {
	service = &Anomaly{
		depth: CDF16Depth,
	}
	err = service.UpdateRequest(settings)
	return
}

// Execute executes the anomaly service
func (a *Anomaly) Execute() (err error) {
	complexity := contexts.Lookup(a.context, a.depth)

	data, err := json.Marshal(a.values)
	if err != nil {
		return
	}
	a.Complexity, a.Count = complexity.Complexity(data)
	return
}

// UpdateRequest updates the SQLD service
func (a *Anomaly) UpdateRequest(values map[string]interface{}) (err error) {
	if context := values["context"]; context != nil {
		a.context = context.(string)
	}

	if depth := values["depth"]; depth != nil {
		a.depth = int(depth.(float64))
	}

	payload := values["payload"]
	if payload == nil {
		return
	}
	b := *payload.(*interface{})
	c, ok := b.(map[string]interface{})
	if !ok {
		return
	}
	a.values = c

	return
}
