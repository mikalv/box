package builder

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"sync"

	"github.com/box-builder/box/builder/executor"
	"github.com/box-builder/box/builder/executor/docker"
	"github.com/box-builder/box/copy"
	"github.com/box-builder/box/global"
	"github.com/box-builder/box/logger"
	"github.com/fatih/color"
	mruby "github.com/mitchellh/go-mruby"
)

var pulls = map[string]chan struct{}{}
var pullMutex = new(sync.Mutex)

// BuildConfig is a struct containing the configuration for the builder.
type BuildConfig struct {
	Globals  *global.Global
	Context  context.Context
	Runner   chan struct{}
	FileName string
}

// BuildResult is an encapsulated tuple of *mruby.MrbValue and error used for
// communicating... build results.
type BuildResult struct {
	FileName string
	Value    *mruby.MrbValue
	Err      error
}

// Builder implements the builder core.
type Builder struct {
	result    BuildResult
	config    *BuildConfig
	mrb       *mruby.Mrb
	exec      executor.Executor
	afterFunc *mruby.MrbValue
}

func (b *Builder) keep(name string) bool {
	for _, fun := range b.config.Globals.OmitFuncs {
		if name == fun {
			return false
		}
	}
	return true
}

// NewBuilder creates a new builder. Returns error on docker or mruby issues.
func NewBuilder(bc BuildConfig) (*Builder, error) {
	if bc.Globals == nil {
		bc.Globals = &global.Global{}
	}

	if !bc.Globals.TTY {
		color.NoColor = true
		copy.NoTTY = true
	}

	if bc.Globals.Logger == nil {
		bc.Globals.Logger = logger.New(bc.FileName, true)
	}

	exec, err := NewExecutor(bc.Context, "docker", bc.Globals)
	if err != nil {
		return nil, err
	}

	builder := &Builder{
		config: &bc,
		mrb:    mruby.NewMrb(),
		exec:   exec,
	}

	for name, def := range verbJumpTable {
		if builder.keep(name) {
			builder.AddVerb(name, def)
		}
	}

	for name, def := range funcJumpTable {
		if builder.keep(name) {
			inner := def.fun
			fn := func(m *mruby.Mrb, self *mruby.MrbValue) (mruby.Value, mruby.Value) {
				return inner(builder, m, self)
			}

			builder.mrb.TopSelf().SingletonClass().DefineMethod(name, fn, def.argSpec)
		}
	}

	return builder, nil
}

// Tag tags the last image yielded by the builder with the provided name.
func (b *Builder) Tag(name string) error {
	return b.exec.Image().Tag(name)
}

// ImageID returns the latest known Image identifier that we committed. At the
// end of the run this will be the golden docker image.
func (b *Builder) ImageID() string {
	return b.exec.Image().ImageID()
}

func (b *Builder) wrapVerbFunc(name string, vd *verbDefinition) mruby.Func {
	return func(m *mruby.Mrb, self *mruby.MrbValue) (mruby.Value, mruby.Value) {
		select {
		case <-b.config.Context.Done():
			if b.config.Context.Err() != nil {
				return nil, createException(m, b.config.Context.Err().Error())
			}

			return nil, nil
		default:
		}

		strArgs := extractStringArgs(m.GetArgs())
		cacheKey := strings.Join(append([]string{name}, strArgs...), ", ")
		cacheKey = base64.StdEncoding.EncodeToString([]byte(cacheKey))

		b.config.Globals.Logger.BuildStep(name, strings.Join(strArgs, ", "))

		if os.Getenv("BOX_DEBUG") != "" {
			content, _ := json.MarshalIndent(b.exec.Config(), "", "  ")
			fmt.Println(string(content))
		}

		cached, err := b.exec.Image().CheckCache(cacheKey)
		if err != nil {
			return nil, createException(m, err.Error())
		}

		// if we don't do this for debug, we will step past it on successive runs
		if !cached || name == "debug" {
			return vd.verbFunc(b, cacheKey, m.GetArgs(), m, self)
		}

		return nil, nil
	}
}

// Config returns the BuildConfig associated with this builder
func (b *Builder) Config() *BuildConfig {
	return b.config
}

// AddVerb adds a function to the mruby dispatch as well as adding hooks around
// the call to ensure containers are committed and intermediate layers are
// cleared.
func (b *Builder) AddVerb(name string, vd *verbDefinition) {
	b.mrb.TopSelf().SingletonClass().DefineMethod(name, b.wrapVerbFunc(name, vd), vd.argSpec)
}

// RunCode runs the ruby value (a proc) and returns the result. It does not
// close the run channel.
func (b *Builder) RunCode(val *mruby.MrbValue, stackKeep int) (BuildResult, int) {
	keep, res, err := b.mrb.RunWithContext(val, b.mrb.TopSelf(), stackKeep)

	b.result = BuildResult{
		FileName: b.FileName(),
		Value:    res,
		Err:      err,
	}

	if err != nil {
		return b.result, keep
	}

	if res != nil {
		b.result = BuildResult{
			FileName: b.FileName(),
			Value:    res,
		}
		return b.result, keep
	}

	if _, err := b.exec.Layers().MakeImage(b.exec.Config()); err != nil {
		b.result.Value = nil
		b.result.Err = err
		return b.result, keep
	}

	_, err = b.mrb.Yield(b.afterFunc)
	if err != nil {
		b.result.Err = err
		return b.result, keep
	}

	b.result.Value = mruby.String(b.ImageID()).MrbValue(b.mrb)
	b.result.Err = nil

	return b.result, keep
}

// Result returns the latest cached result from any run invocation. The
// behavior is undefined if called before any Run()-style invocation.
func (b *Builder) Result() BuildResult {
	return b.result
}

// Run runs the script set by the BuildConfig. It closes the run channel when
// it finishes.
func (b *Builder) Run() BuildResult {
	defer close(b.config.Runner)

	script, err := ioutil.ReadFile(b.config.FileName)
	if err != nil {
		return BuildResult{
			FileName: b.FileName(),
			Err:      err,
		}
	}

	return b.RunScript(string(script))
}

// RunScript runs the provided script. It does not close the run channel.
func (b *Builder) RunScript(script string) BuildResult {
	b.result = BuildResult{
		FileName: b.FileName(),
	}
	if _, err := b.mrb.LoadString(script); err != nil {
		b.result.Err = err
		return b.result
	}

	if _, err := b.exec.Layers().MakeImage(b.exec.Config()); err != nil {
		b.result.Err = err
		return b.result
	}

	if b.afterFunc != nil {
		_, err := b.mrb.Yield(b.afterFunc)
		if err != nil {
			b.result.Err = err
			return b.result
		}
	}

	b.result.Value = mruby.String(b.ImageID()).MrbValue(b.mrb)
	return b.result
}

// Mrb returns the mrb (mruby) instance the builder is using.
func (b *Builder) Mrb() *mruby.Mrb {
	return b.mrb
}

// FileName returns the filename that invoked the build.
func (b *Builder) FileName() string {
	return b.config.FileName
}

// Wait waits for the build to complete.
func (b *Builder) Wait() BuildResult {
	<-b.config.Runner
	return b.result
}

// SetContext sets the execution context.
func (b *Builder) SetContext(ctx context.Context) {
	b.config.Context = ctx
	b.exec.SetContext(ctx)
}

// Close tears down all functions of the builder, preparing it for exit.
func (b *Builder) Close() error {
	b.mrb.EnableGC()
	b.mrb.FullGC()
	b.mrb.Close()
	return nil
}

// NewExecutor returns a valid executor for the given name, or error.
func NewExecutor(ctx context.Context, name string, globals *global.Global) (executor.Executor, error) {
	switch name {
	case "docker":
		return docker.NewDocker(ctx, globals)
	}

	return nil, fmt.Errorf("Executor %q not found", name)
}

// ResetPulls is a function to facilitate testing of the coordinated pull functionality.
func ResetPulls() {
	pulls = map[string]chan struct{}{}
}
