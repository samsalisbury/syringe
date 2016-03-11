// syringe is a lazy dependency injector for Go
package syringe

import (
	"fmt"
	"reflect"
	"sync"
)

type (
	Interface interface {
		Fill(things ...interface{}) error
		Inject(targets ...interface{}) error
	}
	Logger interface {
		Println(...interface{})
	}
	Syringe struct {
		DebugLog       Logger
		objects        map[reflect.Type]reflect.Value
		ctors          map[reflect.Type]ctor
		injectionTypes map[reflect.Type]struct{}
		ctorMutex      sync.Mutex
	}
	ctor struct {
		outType   reflect.Type
		inTypes   []reflect.Type
		construct func(in []reflect.Value) (reflect.Value, error)
		valueChan chan reflect.Value
		errorChan chan error
		once      sync.Once
		done      chan struct{}
		value     *reflect.Value
	}
)

var (
	MaxConcurrency = 10
	DefaultSyringe = (&Syringe{DebugLog: DebugLog}).init()
	DebugLog       Logger
	terror         = reflect.TypeOf((*error)(nil)).Elem()
)

func New() *Syringe {
	return (&Syringe{}).init()
}

func (s *Syringe) init() *Syringe {
	s.objects = map[reflect.Type]reflect.Value{}
	s.ctors = map[reflect.Type]ctor{}
	s.injectionTypes = map[reflect.Type]struct{}{}
	return s
}

func Fill(things ...interface{}) (*Syringe, error) { return DefaultSyringe.Fill(things...) }
func Inject(targets ...interface{}) error          { return DefaultSyringe.Inject(targets...) }

// Fill fills the syringe with objects and constructors. Any function that returns a
// single value, or two return values, the second of which is an error, is considered
// to be a constructor. Everything else is considered to be a fully realised object.
func (s *Syringe) Fill(things ...interface{}) (*Syringe, error) {
	for _, thing := range things {
		s.debugf("Fill: adding a %T", thing)
		if err := s.add(thing); err != nil {
			return s, err
		}
	}
	return s, nil
}

// Inject takes a list of targets, which must be pointers to struct types. It
// tries to inject a value for each field in each target, if a value is known
// for that field's type. All targets, and all fields in each target, are resolved
// concurrently.
func (s *Syringe) Inject(targets ...interface{}) error {
	wg := sync.WaitGroup{}
	wg.Add(len(targets))
	errs := []error{}
	for _, t := range targets {
		go func(target interface{}) {
			defer wg.Done()
			if err := s.inject(target); err != nil {
				s.debugf("error injecting into %T: %s", target, err)
				errs = append(errs, err)
			}
			s.debugf("finished injecting into %T", target)
		}(t)
	}
	wg.Wait()
	if len(errs) != 0 {
		return errs[0]
	}
	return nil
}

// inject just tries to inject a value for each field, no errors if it
// fails, as maybe those other fields are just not meant to receive
// injected values
func (s Syringe) inject(target interface{}) error {
	v := reflect.ValueOf(target)
	ptr := v.Type()
	if ptr.Kind() != reflect.Ptr {
		return fmt.Errorf("got a %s; want a pointer", ptr)
	}
	t := ptr.Elem()
	if t.Kind() != reflect.Struct {
		return fmt.Errorf("got a %s, but %s is not a struct", ptr, t)
	}
	if v.IsNil() {
		return fmt.Errorf("got a %s, but it was nil", ptr)
	}
	nfs := t.NumField()
	wg := sync.WaitGroup{}
	wg.Add(nfs)
	for i := 0; i < nfs; i++ {
		go func(f reflect.Value, fieldName string) {
			defer wg.Done()
			fv, err := s.getValue(f.Type())
			if err == nil {
				f.Set(fv)
				s.debugf("populated %s.%s with %v", t, fieldName, fv)
			}
			s.debugf("not populating %s.%s: %s", t, fieldName, err)
		}(v.Elem().Field(i), t.Field(i).Name)
	}
	wg.Wait()
	return nil
}

func (s *Syringe) add(thing interface{}) error {
	v := reflect.ValueOf(thing)
	s.debugf("registering a %T", thing)
	if v.Kind() == reflect.Ptr && v.IsNil() {
		return fmt.Errorf("thing was nil")
	}
	t := v.Type()
	if c := s.tryMakeCtor(t, v); c != nil {
		return s.addCtor(*c)
	}
	return s.addObject(t, v)
}

func (s *Syringe) getValue(t reflect.Type) (reflect.Value, error) {
	if v, ok := s.objects[t]; ok {
		return v, nil
	}
	c, ok := s.ctors[t]
	if !ok {
		return reflect.Value{}, fmt.Errorf("no constructor for %s", t)
	}
	return c.getValue(s)
}

func (s *Syringe) tryMakeCtor(t reflect.Type, v reflect.Value) *ctor {
	s.debugf("trying to make constructor for %s", t)
	if t.Kind() != reflect.Func {
		return nil
	}
	numOut := t.NumOut()
	if numOut == 0 || numOut > 2 || (numOut == 2 && t.Out(1) != terror) {
		return nil
	}
	outType := t.Out(0)
	numIn := t.NumIn()
	inTypes := make([]reflect.Type, numIn)
	for i := range inTypes {
		inTypes[i] = t.In(i)
	}
	construct := func(in []reflect.Value) (reflect.Value, error) {
		out := v.Call(in)
		var err error
		if len(out) == 2 && !out[1].IsNil() {
			err = out[1].Interface().(error)
		}
		return out[0], err
	}
	return &ctor{outType, inTypes, construct, make(chan reflect.Value), make(chan error), sync.Once{}, make(chan struct{}), nil}
}

func (c ctor) getValue(s *Syringe) (reflect.Value, error) {
	if c.value != nil {
		return *c.value, nil
	}
	c.once.Do(func() {
		wg := sync.WaitGroup{}
		numArgs := len(c.inTypes)
		wg.Add(numArgs)
		args := make([]reflect.Value, numArgs)
		for i, t := range c.inTypes {
			i, t := i, t
			go func() {
				defer wg.Done()
				v, err := s.getValue(t)
				if err != nil {
					c.errorChan <- err
				}
				args[i] = v
			}()
		}
		wg.Wait()
		v, err := c.construct(args)
		if err != nil {
			c.errorChan <- err
		}
		c.value = &v
		close(c.errorChan)
	})
	if err := <-c.errorChan; err != nil {
		return reflect.Value{}, err
	}
	return *c.value, nil
}

func (s *Syringe) addCtor(c ctor) error {
	if err := s.registerInjectionType(c.outType); err != nil {
		return err
	}
	s.ctors[c.outType] = c
	return nil
}

func (s *Syringe) addObject(t reflect.Type, v reflect.Value) error {
	if err := s.registerInjectionType(t); err != nil {
		return err
	}
	s.objects[t] = v
	return nil
}

func (s *Syringe) registerInjectionType(t reflect.Type) error {
	if _, alreadyRegistered := s.injectionTypes[t]; alreadyRegistered {
		return fmt.Errorf("injection type %s already registered", t)
	}
	s.injectionTypes[t] = struct{}{}
	return nil
}

func (s *Syringe) debugf(format string, a ...interface{}) {
	if s.DebugLog == nil {
		return
	}
	s.DebugLog.Println(fmt.Sprintf(format, a...))
}
