// Copyright 2015 The Vanadium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"v.io/x/devtools/internal/collect"
	"v.io/x/devtools/internal/goutil"
	"v.io/x/devtools/internal/runutil"
	"v.io/x/devtools/internal/test"
	"v.io/x/devtools/internal/tool"
	"v.io/x/devtools/internal/util"
	"v.io/x/devtools/internal/xunit"
	"v.io/x/lib/host"
)

type taskStatus int

const (
	buildPassed taskStatus = iota
	buildFailed
	testPassed
	testFailed
	testTimedout
)

type buildResult struct {
	pkg    string
	status taskStatus
	output string
	time   time.Duration
}

type goBuildOpt interface {
	goBuildOpt()
}

type goCoverageOpt interface {
	goCoverageOpt()
}

type goTestOpt interface {
	goTestOpt()
}

type funcMatcherOpt struct{ funcMatcher }

type nonTestArgsOpt []string
type argsOpt []string
type timeoutOpt string
type suffixOpt string
type exclusionsOpt []exclusion
type pkgsOpt []string
type numWorkersOpt int

func (argsOpt) goBuildOpt()    {}
func (argsOpt) goCoverageOpt() {}
func (argsOpt) goTestOpt()     {}

func (nonTestArgsOpt) goTestOpt() {}

func (timeoutOpt) goCoverageOpt() {}
func (timeoutOpt) goTestOpt()     {}

func (suffixOpt) goTestOpt() {}

func (exclusionsOpt) goTestOpt() {}

func (funcMatcherOpt) goTestOpt() {}

func (pkgsOpt) goTestOpt()     {}
func (pkgsOpt) goBuildOpt()    {}
func (pkgsOpt) goCoverageOpt() {}

func (numWorkersOpt) goTestOpt() {}

// goBuild is a helper function for running Go builds.
func goBuild(ctx *tool.Context, testName string, opts ...goBuildOpt) (_ *test.Result, e error) {
	args, pkgs := []string{}, []string{}
	for _, opt := range opts {
		switch typedOpt := opt.(type) {
		case argsOpt:
			args = []string(typedOpt)
		case pkgsOpt:
			pkgs = []string(typedOpt)
		}
	}

	// Enumerate the packages to be built.
	pkgList, err := goutil.List(ctx, pkgs)
	if err != nil {
		return nil, err
	}

	// Create a pool of workers.
	numPkgs := len(pkgList)
	tasks := make(chan string, numPkgs)
	taskResults := make(chan buildResult, numPkgs)
	for i := 0; i < runtime.NumCPU(); i++ {
		go buildWorker(ctx, args, tasks, taskResults)
	}

	// Distribute work to workers.
	for _, pkg := range pkgList {
		tasks <- pkg
	}
	close(tasks)

	// Collect the results.
	allPassed, suites := true, []xunit.TestSuite{}
	for i := 0; i < numPkgs; i++ {
		result := <-taskResults
		s := xunit.TestSuite{Name: result.pkg}
		c := xunit.TestCase{
			Classname: result.pkg,
			Name:      "Build",
			Time:      fmt.Sprintf("%.2f", result.time.Seconds()),
		}
		if result.status != buildPassed {
			test.Fail(ctx, "%s\n%v\n", result.pkg, result.output)
			f := xunit.Failure{
				Message: "build",
				Data:    result.output,
			}
			c.Failures = append(c.Failures, f)
			allPassed = false
			s.Failures++
		} else {
			test.Pass(ctx, "%s\n", result.pkg)
		}
		s.Tests++
		s.Cases = append(s.Cases, c)
		suites = append(suites, s)
	}
	close(taskResults)

	// Create the xUnit report.
	if err := xunit.CreateReport(ctx, testName, suites); err != nil {
		return nil, err
	}
	if !allPassed {
		return &test.Result{Status: test.Failed}, nil
	}
	return &test.Result{Status: test.Passed}, nil
}

// buildWorker builds packages.
func buildWorker(ctx *tool.Context, args []string, pkgs <-chan string, results chan<- buildResult) {
	opts := ctx.Run().Opts()
	opts.Verbose = false
	for pkg := range pkgs {
		var out bytes.Buffer
		args := append([]string{"go", "build", "-o", filepath.Join(binDirPath(), path.Base(pkg))}, args...)
		args = append(args, pkg)
		opts.Stdout = &out
		opts.Stderr = &out
		start := time.Now()
		err := ctx.Run().CommandWithOpts(opts, "v23", args...)
		duration := time.Now().Sub(start)
		result := buildResult{
			pkg:    pkg,
			time:   duration,
			output: out.String(),
		}
		if err != nil {
			result.status = buildFailed
		} else {
			result.status = buildPassed
		}
		results <- result
	}
}

type coverageResult struct {
	pkg      string
	coverage *os.File
	output   string
	status   taskStatus
	time     time.Duration
}

const defaultTestCoverageTimeout = "5m"

// goCoverage is a helper function for running Go coverage tests.
func goCoverage(ctx *tool.Context, testName string, opts ...goCoverageOpt) (_ *test.Result, e error) {
	timeout := defaultTestCoverageTimeout
	args, pkgs := []string{}, []string{}
	for _, opt := range opts {
		switch typedOpt := opt.(type) {
		case timeoutOpt:
			timeout = string(typedOpt)
		case argsOpt:
			args = []string(typedOpt)
		case pkgsOpt:
			pkgs = []string(typedOpt)
		}
	}

	// Install dependencies.
	if err := installGoCover(ctx); err != nil {
		return nil, err
	}
	if err := installGoCoverCobertura(ctx); err != nil {
		return nil, err
	}
	if err := installGo2XUnit(ctx); err != nil {
		return nil, err
	}

	// Pre-build non-test packages.
	if err := buildTestDeps(ctx, pkgs); err != nil {
		if err := xunit.CreateFailureReport(ctx, testName, "BuildTestDependencies", "TestCoverage", "dependencies build failure", err.Error()); err != nil {
			return nil, err
		}
		return &test.Result{Status: test.Failed}, nil
	}

	// Enumerate the packages for which coverage is to be computed.
	pkgList, err := goutil.List(ctx, pkgs)
	if err != nil {
		return nil, err
	}

	// Create a pool of workers.
	numPkgs := len(pkgList)
	tasks := make(chan string, numPkgs)
	taskResults := make(chan coverageResult, numPkgs)
	for i := 0; i < runtime.NumCPU(); i++ {
		go coverageWorker(ctx, timeout, args, tasks, taskResults)
	}

	// Distribute work to workers.
	for _, pkg := range pkgList {
		tasks <- pkg
	}
	close(tasks)

	// Collect the results.
	//
	// TODO(jsimsa): Gather coverage data using the testCoverage
	// data structure as opposed to a buffer.
	var coverageData bytes.Buffer
	fmt.Fprintf(&coverageData, "mode: set\n")
	allPassed, suites := true, []xunit.TestSuite{}
	for i := 0; i < numPkgs; i++ {
		result := <-taskResults
		var s *xunit.TestSuite
		switch result.status {
		case buildFailed:
			s = xunit.CreateTestSuiteWithFailure(result.pkg, "TestCoverage", "build failure", result.output, result.time)
		case testPassed:
			data, err := ioutil.ReadAll(result.coverage)
			if err != nil {
				return nil, err
			}
			lines := strings.Split(string(data), "\n")
			for _, line := range lines {
				if line != "" && strings.Index(line, "mode: set") == -1 {
					fmt.Fprintf(&coverageData, "%s\n", line)
				}
			}
			fallthrough
		case testFailed:
			if strings.Index(result.output, "no test files") == -1 {
				ss, err := xunit.TestSuiteFromGoTestOutput(ctx, bytes.NewBufferString(result.output))
				if err != nil {
					// Token too long error.
					if !strings.HasSuffix(err.Error(), "token too long") {
						return nil, err
					}
					ss = xunit.CreateTestSuiteWithFailure(result.pkg, "Test", "test output contains lines that are too long to parse", "", result.time)
				}
				s = ss
			}
		}
		if result.coverage != nil {
			result.coverage.Close()
			if err := ctx.Run().RemoveAll(result.coverage.Name()); err != nil {
				return nil, err
			}
		}
		if s != nil {
			if s.Failures > 0 {
				allPassed = false
				test.Fail(ctx, "%s\n%v\n", result.pkg, result.output)
			} else {
				test.Pass(ctx, "%s\n", result.pkg)
			}
			suites = append(suites, *s)
		}
	}
	close(taskResults)

	// Create the xUnit and cobertura reports.
	if err := xunit.CreateReport(ctx, testName, suites); err != nil {
		return nil, err
	}
	coverage, err := coverageFromGoTestOutput(ctx, &coverageData)
	if err != nil {
		return nil, err
	}
	if err := createCoberturaReport(ctx, testName, coverage); err != nil {
		return nil, err
	}
	if !allPassed {
		return &test.Result{Status: test.Failed}, nil
	}
	return &test.Result{Status: test.Passed}, nil
}

// coverageWorker generates test coverage.
func coverageWorker(ctx *tool.Context, timeout string, args []string, pkgs <-chan string, results chan<- coverageResult) {
	opts := ctx.Run().Opts()
	opts.Verbose = false
	for pkg := range pkgs {
		// Compute the test coverage.
		var out bytes.Buffer
		coverageFile, err := ioutil.TempFile("", "")
		if err != nil {
			panic(fmt.Sprintf("TempFile() failed: %v", err))
		}
		args := append([]string{
			"go", "test", "-cover", "-coverprofile",
			coverageFile.Name(), "-timeout", timeout, "-v",
		}, args...)
		args = append(args, pkg)
		opts.Stdout = &out
		opts.Stderr = &out
		start := time.Now()
		err = ctx.Run().CommandWithOpts(opts, "v23", args...)
		result := coverageResult{
			pkg:      pkg,
			coverage: coverageFile,
			time:     time.Now().Sub(start),
			output:   out.String(),
		}
		if err != nil {
			if isBuildFailure(err, out.String(), pkg) {
				result.status = buildFailed
			} else {
				result.status = testFailed
			}
		} else {
			result.status = testPassed
		}
		results <- result
	}
}

// funcMatcher is the interface for determing if functions in the loaded ast
// of a package match a certain criteria.
type funcMatcher interface {
	match(*ast.FuncDecl) (bool, string)
}

type matchGoTestFunc struct{}

func (t *matchGoTestFunc) match(fn *ast.FuncDecl) (bool, string) {
	name := fn.Name.String()
	// TODO(cnicolaou): match on signature, not just name.
	return strings.HasPrefix(name, "Test"), name
}
func (t *matchGoTestFunc) goTestOpt() {}

type matchV23TestFunc struct{}

func (t *matchV23TestFunc) match(fn *ast.FuncDecl) (bool, string) {
	name := fn.Name.String()
	if !strings.HasPrefix(name, "V23Test") {
		return false, name
	}
	sig := fn.Type
	if len(sig.Params.List) != 1 || sig.Results != nil {
		return false, name
	}
	typ := sig.Params.List[0].Type
	star, ok := typ.(*ast.StarExpr)
	if !ok {
		return false, name
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return false, name
	}
	return pkgIdent.Name == "v23tests" && sel.Sel.Name == "T", name
}

func (t *matchV23TestFunc) goTestOpt() {}

// goListPackagesAndFuncs is a helper function for listing Go
// packages and obtaining lists of function names that are matched
// by the matcher interface.
func goListPackagesAndFuncs(ctx *tool.Context, pkgs []string, matcher funcMatcher) ([]string, map[string][]string, error) {

	env, err := util.VanadiumEnvironment(ctx, util.HostPlatform())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to obtain the Vanadium environment: %v", err)
	}
	pkgList, err := goutil.List(ctx, pkgs)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list packages: %v", err)
	}

	matched := map[string][]string{}
	pkgsWithTests := []string{}

	buildContext := build.Default
	buildContext.GOPATH = env.Get("GOPATH")
	for _, pkg := range pkgList {
		pi, err := buildContext.Import(pkg, ".", build.ImportMode(0))
		if err != nil {
			return nil, nil, err
		}
		testFiles := append(pi.TestGoFiles, pi.XTestGoFiles...)
		fset := token.NewFileSet() // positions are relative to fset
		for _, testFile := range testFiles {
			file := filepath.Join(pi.Dir, testFile)
			testAST, err := parser.ParseFile(fset, file, nil, parser.Mode(0))
			if err != nil {
				return nil, nil, fmt.Errorf("failed parsing: %v: %v", file, err)
			}
			for _, decl := range testAST.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if ok, result := matcher.match(fn); ok {
					matched[pkg] = append(matched[pkg], result)
				}
			}
		}
		if len(matched[pkg]) > 0 {
			pkgsWithTests = append(pkgsWithTests, pkg)
		}
	}
	return pkgsWithTests, matched, nil
}

// filterExcludedTests filters out excluded tests returning an
// indication of whether this package should be included in test runs
// and a list of the specific tests that should be run (which if nil
// means running all of the tests), and a list of the skipped tests.
func filterExcludedTests(pkg string, testNames []string, exclusions []exclusion) (bool, []string, []string) {
	excluded := []string{}
	for _, name := range testNames {
		for _, exclusion := range exclusions {
			if exclusion.pkgRE.MatchString(pkg) && exclusion.nameRE.MatchString(name) {
				excluded = append(excluded, name)
				break
			}
		}
	}
	if len(excluded) == 0 {
		// Run all of the tests, none are to be skipped/excluded.
		return true, nil, nil
	}

	remaining := []string{}
	for _, name := range testNames {
		found := false
		for _, exclude := range excluded {
			if name == exclude {
				found = true
				break
			}
		}
		if !found {
			remaining = append(remaining, name)
		}
	}
	return len(remaining) > 0, remaining, excluded
}

type testResult struct {
	pkg      string
	output   string
	excluded []string
	status   taskStatus
	time     time.Duration
}

const defaultTestTimeout = "5m"

type goTestTask struct {
	pkg string
	// specificTests enumerates the tests to run:
	// if non-nil, pass to -run as a regex or'ing each item in the slice.
	// if nil, invoke the test without -run.
	specificTests []string
	// excludedTests enumerates the tests that are to be excluded as a result
	// of exclusion rules.
	excludedTests []string
}

// goTest is a helper function for running Go tests.
func goTest(ctx *tool.Context, testName string, opts ...goTestOpt) (_ *test.Result, e error) {
	timeout := defaultTestTimeout
	args, suffix, exclusions, pkgs := []string{}, "", []exclusion{}, []string{}
	var matcher funcMatcher
	matcher = &matchGoTestFunc{}
	numWorkers := runtime.NumCPU()
	var nonTestArgs nonTestArgsOpt
	for _, opt := range opts {
		switch typedOpt := opt.(type) {
		case timeoutOpt:
			timeout = string(typedOpt)
		case argsOpt:
			args = []string(typedOpt)
		case suffixOpt:
			suffix = string(typedOpt)
		case exclusionsOpt:
			exclusions = []exclusion(typedOpt)
		case nonTestArgsOpt:
			nonTestArgs = typedOpt
		case funcMatcherOpt:
			matcher = typedOpt
		case pkgsOpt:
			pkgs = []string(typedOpt)
		case numWorkersOpt:
			numWorkers = int(typedOpt)
			if numWorkers < 1 {
				numWorkers = 1
			}

		}
	}

	// Install dependencies.
	if err := installGo2XUnit(ctx); err != nil {
		return nil, err
	}

	// Pre-build non-test packages.
	if err := buildTestDeps(ctx, pkgs); err != nil {
		originalTestName := testName
		if len(suffix) != 0 {
			testName += " " + suffix
		}
		if err := xunit.CreateFailureReport(ctx, originalTestName, "BuildTestDependencies", testName, "dependencies build failure", err.Error()); err != nil {
			return nil, err
		}
		return &test.Result{Status: test.Failed}, nil
	}

	// Enumerate the packages and tests to be built.
	pkgList, pkgAndFuncList, err := goListPackagesAndFuncs(ctx, pkgs, matcher)
	if err != nil {
		return nil, err
	}

	// Create a pool of workers.
	numPkgs := len(pkgList)
	tasks := make(chan goTestTask, numPkgs)
	taskResults := make(chan testResult, numPkgs)

	fmt.Fprintf(ctx.Stdout(), "Running tests using %d workers...\n", numWorkers)
	fmt.Fprintf(ctx.Stdout(), "Running tests concurrently...\n")
	staggeredWorker := func() {
		delay := time.Duration(rand.Int63n(30*1000)) * time.Millisecond
		if ctx.Verbose() {
			fmt.Fprintf(ctx.Stdout(), "Staggering start of test worker by %s\n", delay)
		}
		time.Sleep(delay)
		testWorker(ctx, timeout, args, nonTestArgs, tasks, taskResults)
	}
	for i := 0; i < numWorkers; i++ {
		if numWorkers > 1 {
			go staggeredWorker()
		} else {
			go testWorker(ctx, timeout, args, nonTestArgs, tasks, taskResults)
		}
	}

	// Distribute work to workers.
	for _, pkg := range pkgList {
		testThisPkg, specificTests, excludedTests := filterExcludedTests(pkg, pkgAndFuncList[pkg], exclusions)
		if testThisPkg {
			tasks <- goTestTask{pkg, specificTests, excludedTests}
		} else {
			taskResults <- testResult{
				pkg:      pkg,
				output:   "package excluded",
				excluded: excludedTests,
				status:   testPassed,
			}
		}
	}
	close(tasks)

	// Collect the results.

	// excludedTests are a result of exclusion rules in this tool.
	excludedTests := map[string][]string{}
	// skippedTests are a result of testing.Skip calls in the actual
	// tests.
	skippedTests := map[string][]string{}
	allPassed, suites := true, []xunit.TestSuite{}
	for i := 0; i < numPkgs; i++ {
		result := <-taskResults
		var s *xunit.TestSuite
		switch result.status {
		case buildFailed:
			s = xunit.CreateTestSuiteWithFailure(result.pkg, "Test", "build failure", result.output, result.time)
		case testFailed, testPassed:
			if strings.Index(result.output, "no test files") == -1 &&
				strings.Index(result.output, "package excluded") == -1 {
				ss, err := xunit.TestSuiteFromGoTestOutput(ctx, bytes.NewBufferString(result.output))
				if err != nil {
					// Token too long error.
					if !strings.HasSuffix(err.Error(), "token too long") {
						return nil, err
					}
					ss = xunit.CreateTestSuiteWithFailure(result.pkg, "Test", "test output contains lines that are too long to parse", "", result.time)
				}
				if ss.Skip > 0 {
					for _, c := range ss.Cases {
						if c.Skipped != nil {
							skippedTests[result.pkg] = append(skippedTests[result.pkg], c.Name)
						}
					}
				}
				s = ss
			}
			if len(result.excluded) > 0 {
				excludedTests[result.pkg] = result.excluded
			}
		}
		if s != nil {
			if s.Failures > 0 {
				allPassed = false
				test.Fail(ctx, "%s\n%v\n", result.pkg, result.output)
			} else {
				test.Pass(ctx, "%s\n", result.pkg)
			}
			if s.Skip > 0 {
				test.Pass(ctx, "%s (skipped tests: %v)\n", result.pkg, skippedTests[result.pkg])
			}

			newCases := []xunit.TestCase{}
			for _, c := range s.Cases {
				if len(suffix) != 0 {
					c.Name += " " + suffix
				}
				newCases = append(newCases, c)
			}
			s.Cases = newCases
			suites = append(suites, *s)
		}
		if excluded := excludedTests[result.pkg]; excluded != nil {
			test.Pass(ctx, "%s (excluded tests: %v)\n", result.pkg, excluded)
		}
	}
	close(taskResults)

	// Create the xUnit report.
	if err := xunit.CreateReport(ctx, testName, suites); err != nil {
		return nil, err
	}
	testResult := &test.Result{
		Status:        test.Passed,
		ExcludedTests: excludedTests,
		SkippedTests:  skippedTests,
	}
	if !allPassed {
		testResult.Status = test.Failed
	}
	return testResult, nil
}

// testWorker tests packages.
func testWorker(ctx *tool.Context, timeout string, args, nonTestArgs []string, tasks <-chan goTestTask, results chan<- testResult) {
	opts := ctx.Run().Opts()
	opts.Verbose = false
	for task := range tasks {
		// Run the test.
		taskArgs := append([]string{"go", "test", "-timeout", timeout, "-v"}, args...)
		if len(task.specificTests) != 0 {
			taskArgs = append(taskArgs, "-run", fmt.Sprintf("%s", strings.Join(task.specificTests, "|")))
		}
		taskArgs = append(taskArgs, task.pkg)
		taskArgs = append(taskArgs, nonTestArgs...)
		var out bytes.Buffer
		opts.Stdout = &out
		opts.Stderr = &out
		start := time.Now()
		timeoutDuration, err := time.ParseDuration(timeout)
		if err != nil {
			results <- testResult{
				status:   testFailed,
				pkg:      task.pkg,
				output:   fmt.Sprintf("time.ParseDuration(%s) failed: %v", timeout, err),
				excluded: task.excludedTests,
			}
			continue
		}
		err = ctx.Run().TimedCommandWithOpts(timeoutDuration, opts, "v23", taskArgs...)
		result := testResult{
			pkg:      task.pkg,
			time:     time.Now().Sub(start),
			output:   out.String(),
			excluded: task.excludedTests,
		}
		if err != nil {
			if isBuildFailure(err, out.String(), task.pkg) {
				result.status = buildFailed
			} else if err == runutil.CommandTimedOutErr {
				result.status = testTimedout
			} else {
				result.status = testFailed
			}
		} else {
			result.status = testPassed
		}
		results <- result
	}
}

// buildTestDeps builds dependencies for the given test packages
func buildTestDeps(ctx *tool.Context, pkgs []string) error {
	fmt.Fprintf(ctx.Stdout(), "building test dependencies ... ")
	args := append([]string{"go", "test", "-i"}, pkgs...)
	var out bytes.Buffer
	opts := ctx.Run().Opts()
	opts.Stderr = &out
	err := ctx.Run().CommandWithOpts(opts, "v23", args...)
	if err == nil {
		fmt.Fprintf(ctx.Stdout(), "ok\n")
		return nil
	}
	fmt.Fprintf(ctx.Stdout(), "failed\n%s\n", out.String())
	return fmt.Errorf("%v\n%s", err, out.String())
}

// installGoCover makes sure the "go cover" tool is installed.
//
// TODO(jsimsa): Unify the installation functions by moving the
// gocover-cobertura and go2xunit tools into the third_party
// project.
func installGoCover(ctx *tool.Context) error {
	// Check if the tool exists.
	var out bytes.Buffer
	cmd := exec.Command("go", "tool")
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		if scanner.Text() == "cover" {
			return nil
		}
	}
	if scanner.Err() != nil {
		return fmt.Errorf("Scan() failed: %v")
	}
	if err := ctx.Run().Command("v23", "go", "install", "golang.org/x/tools/cmd/cover"); err != nil {
		return err
	}
	return nil
}

// installGoCoverCobertura makes sure the "gocover-cobertura" tool is
// installed.
func installGoCoverCobertura(ctx *tool.Context) error {
	root, err := util.V23Root()
	if err != nil {
		return err
	}
	// Check if the tool exists.
	bin, err := util.ThirdPartyBinPath(root, "gocover-cobertura")
	if err != nil {
		return err
	}
	if _, err := os.Stat(bin); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		opts := ctx.Run().Opts()
		if err := ctx.Run().CommandWithOpts(opts, "v23", "go", "install", "github.com/t-yuki/gocover-cobertura"); err != nil {
			return err
		}
	}
	return nil
}

// installGo2XUnit makes sure the "go2xunit" tool is installed.
func installGo2XUnit(ctx *tool.Context) error {
	root, err := util.V23Root()
	if err != nil {
		return err
	}
	// Check if the tool exists.
	bin, err := util.ThirdPartyBinPath(root, "go2xunit")
	if err != nil {
		return err
	}
	if _, err := os.Stat(bin); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		opts := ctx.Run().Opts()
		if err := ctx.Run().CommandWithOpts(opts, "v23", "go", "install", "bitbucket.org/tebeka/go2xunit"); err != nil {
			return err
		}
	}
	return nil
}

// isBuildFailure checks whether the given error and output indicate a build failure for the given package.
func isBuildFailure(err error, out, pkg string) bool {
	if exitError, ok := err.(*exec.ExitError); ok {
		// Try checking err's process state to determine the exit code.
		// Exit code 2 means build failures.
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok {
			exitCode := status.ExitStatus()
			// A exit code of 2 means build failure.
			if exitCode == 2 {
				return true
			}
			// When the exit code is 1, we need to check the output to distinguish
			// "setup failure" and "test failure".
			if exitCode == 1 {
				// Treat setup failure as build failure.
				if strings.HasPrefix(out, fmt.Sprintf("# %s", pkg)) &&
					strings.HasSuffix(out, "[setup failed]\n") {
					return true
				}
				return false
			}
		}
	}
	// As a fallback, check the output line.
	// If the output starts with "# ${pkg}", then it should be a build failure.
	return strings.HasPrefix(out, fmt.Sprintf("# %s", pkg))
}

// getListenerPID finds the process ID of the process listening on the
// given port. If no process is listening on the given port (or an
// error is encountered), the function returns -1.
func getListenerPID(ctx *tool.Context, port string) (int, error) {
	// Make sure "lsof" exists.
	_, err := exec.LookPath("lsof")
	if err != nil {
		return -1, fmt.Errorf(`"lsof" not found in the PATH`)
	}

	// Use "lsof" to find the process ID of the listener.
	var out bytes.Buffer
	opts := ctx.Run().Opts()
	opts.Stdout = &out
	opts.Stderr = &out
	if err := ctx.Run().CommandWithOpts(opts, "lsof", "-i", ":"+port, "-sTCP:LISTEN", "-F", "p"); err != nil {
		// When no listener exists, "lsof" exits with non-zero
		// status.
		return -1, nil
	}

	// Parse the port number.
	pidString := strings.TrimPrefix(strings.TrimSpace(out.String()), "p")
	pid, err := strconv.Atoi(pidString)
	if err != nil {
		return -1, fmt.Errorf("Atoi(%v) failed: %v", pidString, err)
	}

	return pid, nil
}

type exclusion struct {
	exclude bool
	nameRE  *regexp.Regexp
	pkgRE   *regexp.Regexp
}

// newExclusion is the exclusion factory.
func newExclusion(pkg, name string, exclude bool) exclusion {
	return exclusion{
		exclude: exclude,
		nameRE:  regexp.MustCompile(name),
		pkgRE:   regexp.MustCompile(pkg),
	}
}

var (
	goExclusions     []exclusion
	goRaceExclusions []exclusion
)

func init() {
	goExclusions = []exclusion{
		// This test triggers a bug in go 1.4.1 garbage collector.
		//
		// https://github.com/veyron/release-issues/issues/1494
		newExclusion("v.io/x/ref/profiles/internal/rpc/stream/vc", "TestConcurrentFlows", isDarwin() && is386()),
		// The fsnotify package tests are flaky on darwin. This begs the
		// question of whether we should be relying on this library at
		// all.
		newExclusion("github.com/howeyc/fsnotify", ".*", isDarwin()),
		// These tests are not maintained very well and are broken on all
		// platforms.
		//
		// TODO(spetrovic): Put these back in once the owners fixes them.
		newExclusion("golang.org/x/mobile", ".*", true),
		// The following test requires IPv6, which is not available on
		// some of our continuous integration instances.
		newExclusion("golang.org/x/net/icmp", "TestPingGoogle", isCI()),
		// Don't run this test on mac systems prior to Yosemite since it
		// can crash some machines.
		newExclusion("golang.org/x/net/ipv6", ".*", !isYosemite()),
		// The following test is way out of date and doesn't work any more.
		newExclusion("golang.org/x/tools", "TestCheck", true),
		// The following two tests use too much memory.
		newExclusion("golang.org/x/tools/go/loader", "TestStdlib", true),
		newExclusion("golang.org/x/tools/go/ssa", "TestStdlib", true),
		// The following test expects to see "FAIL: TestBar" which causes
		// go2xunit to fail.
		newExclusion("golang.org/x/tools/go/ssa/interp", "TestTestmainPackage", true),
		// More broken tests.
		//
		// TODO(jsimsa): Provide more descriptive message.
		newExclusion("golang.org/x/tools/go/types", "TestCheck", true),
		newExclusion("golang.org/x/tools/refactor/lexical", "TestStdlib", true),
		newExclusion("golang.org/x/tools/refactor/importgraph", "TestBuild", true),
		// The godoc test does some really stupid string matching where it doesn't want
		// cmd/gc to appear, but we have v.io/x/ref/cmd/gclogs.
		newExclusion("golang.org/x/tools/cmd/godoc", "TestWeb", true),
		// The mysql tests require a connection to a MySQL database.
		newExclusion("github.com/go-sql-driver/mysql", ".*", true),
		// The gorp tests require a connection to a SQL database, configured
		// through various environment variables.
		newExclusion("github.com/go-gorp/gorp", ".*", true),
		// The check.v1 tests contain flakey benchmark tests which sometimes do
		// not complete, and sometimes complete with unexpected times.
		newExclusion("gopkg.in/check.v1", ".*", true),
	}

	// Tests excluded only when running under --race flag.
	goRaceExclusions = []exclusion{
		// This test takes too long in --race mode.
		newExclusion("v.io/x/devtools/v23", "TestV23Generate", true),
	}
}

// ExcludedTests returns the set of tests to be excluded from the
// tests executed when testing the Vanadium project.
func ExcludedTests() []string {
	return excludedTests(goExclusions)
}

// ExcludedRaceTests returns the set of race tests to be excluded from
// the tests executed when testing the Vanadium project.
func ExcludedRaceTests() []string {
	return excludedTests(goRaceExclusions)
}

func excludedTests(exclusions []exclusion) []string {
	excluded := make([]string, 0, len(exclusions))
	for _, e := range exclusions {
		if e.exclude {
			excluded = append(excluded, fmt.Sprintf("pkg: %v, name: %v", e.pkgRE.String(), e.nameRE.String()))
		}
	}
	return excluded
}

// validateAgainstDefaultPackages makes sure that the packages requested
// via opts are amongst the defaults assuming that all of the defaults are
// specified in <pkg>/... form and returns one of each of the goBuildOpt,
// goCoverageOpt and goTestOpt options.
// If no packages are requested, the defaults are returned.
// TODO(cnicolaou): ideally there'd be one piece of code that understands
//   go package specifications that could be used here.
func validateAgainstDefaultPackages(ctx *tool.Context, opts []Opt, defaults []string) (pkgsOpt, error) {

	optPkgs := []string{}
	for _, opt := range opts {
		switch v := opt.(type) {
		case PkgsOpt:
			optPkgs = []string(v)
		}
	}

	if len(optPkgs) == 0 {
		defsOpt := pkgsOpt(defaults)
		return defsOpt, nil
	}

	defPkgs, err := goutil.List(ctx, defaults)
	if err != nil {
		return nil, err
	}

	pkgs, err := goutil.List(ctx, optPkgs)
	if err != nil {
		return nil, err
	}

	for _, p := range pkgs {
		found := false
		for _, d := range defPkgs {
			if p == d {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("requested packages %v is not one of %v", p, defaults)
		}
	}
	po := pkgsOpt(pkgs)
	return po, nil
}

// getNumWorkersOpt gets the NumWorkersOpt from the given Opt slice
func getNumWorkersOpt(opts []Opt) numWorkersOpt {
	for _, opt := range opts {
		switch v := opt.(type) {
		case NumWorkersOpt:
			return numWorkersOpt(v)
		}
	}
	return numWorkersOpt(runtime.NumCPU())
}

// thirdPartyGoBuild runs Go build for third-party projects.
func thirdPartyGoBuild(ctx *tool.Context, testName string, opts ...Opt) (_ *test.Result, e error) {
	// Initialize the test.
	cleanup, err := initTest(ctx, testName, []string{})
	if err != nil {
		return nil, internalTestError{err, "Init"}
	}
	defer collect.Error(func() error { return cleanup() }, &e)

	// Build the third-party Go packages.
	pkgs, err := thirdPartyPkgs()
	if err != nil {
		return nil, err
	}
	validatedPkgs, err := validateAgainstDefaultPackages(ctx, opts, pkgs)
	if err != nil {
		return nil, err
	}
	return goBuild(ctx, testName, validatedPkgs)
}

// thirdPartyGoTest runs Go tests for the third-party projects.
func thirdPartyGoTest(ctx *tool.Context, testName string, opts ...Opt) (_ *test.Result, e error) {
	// Initialize the test.
	cleanup, err := initTest(ctx, testName, []string{})
	if err != nil {
		return nil, internalTestError{err, "Init"}
	}
	defer collect.Error(func() error { return cleanup() }, &e)

	// Test the third-party Go packages.
	pkgs, err := thirdPartyPkgs()
	if err != nil {
		return nil, err
	}
	validatedPkgs, err := validateAgainstDefaultPackages(ctx, opts, pkgs)
	if err != nil {
		return nil, err
	}
	suffix := suffixOpt(genTestNameSuffix("GoTest"))
	return goTest(ctx, testName, suffix, exclusionsOpt(goExclusions), validatedPkgs)
}

// thirdPartyGoRace runs Go data-race tests for third-party projects.
func thirdPartyGoRace(ctx *tool.Context, testName string, opts ...Opt) (_ *test.Result, e error) {
	// Initialize the test.
	cleanup, err := initTest(ctx, testName, []string{})
	if err != nil {
		return nil, internalTestError{err, "Init"}
	}
	defer collect.Error(func() error { return cleanup() }, &e)

	// Test the third-party Go packages for data races.
	pkgs, err := thirdPartyPkgs()
	if err != nil {
		return nil, err
	}
	validatedPkgs, err := validateAgainstDefaultPackages(ctx, opts, pkgs)
	if err != nil {
		return nil, err
	}
	args := argsOpt([]string{"-race"})
	exclusions := append(goExclusions, goRaceExclusions...)
	suffix := suffixOpt(genTestNameSuffix("GoRace"))
	return goTest(ctx, testName, suffix, args, exclusionsOpt(exclusions), validatedPkgs)
}

// thirdPartyPkgs returns a list of Go expressions that describe all
// third-party packages.
func thirdPartyPkgs() ([]string, error) {
	root, err := util.V23Root()
	if err != nil {
		return nil, err
	}

	thirdPartyDir := filepath.Join(root, "third_party", "go", "src")
	fileInfos, err := ioutil.ReadDir(thirdPartyDir)
	if err != nil {
		return nil, fmt.Errorf("ReadDir(%v) failed: %v", thirdPartyDir, err)
	}

	pkgs := []string{}
	for _, fileInfo := range fileInfos {
		if fileInfo.IsDir() {
			pkgs = append(pkgs, fileInfo.Name()+"/...")
		}
	}
	return pkgs, nil
}

// vanadiumGoBench runs Go benchmarks for vanadium projects.
func vanadiumGoBench(ctx *tool.Context, testName string, opts ...Opt) (_ *test.Result, e error) {
	// Initialize the test.
	cleanup, err := initTest(ctx, testName, []string{})
	if err != nil {
		return nil, internalTestError{err, "Init"}
	}
	defer collect.Error(func() error { return cleanup() }, &e)

	// Benchmark the Vanadium Go packages.
	pkgs, err := validateAgainstDefaultPackages(ctx, opts, []string{"v.io/..."})
	if err != nil {
		return nil, err
	}
	args := argsOpt([]string{"-bench", ".", "-run", "XXX"})
	return goTest(ctx, testName, args, pkgs)
}

// vanadiumGoBuild runs Go build for the vanadium projects.
func vanadiumGoBuild(ctx *tool.Context, testName string, opts ...Opt) (_ *test.Result, e error) {
	// Initialize the test.
	cleanup, err := initTest(ctx, testName, []string{})
	if err != nil {
		return nil, internalTestError{err, "Init"}
	}

	// Build the Vanadium Go packages.
	defer collect.Error(func() error { return cleanup() }, &e)
	pkgs, err := validateAgainstDefaultPackages(ctx, opts, []string{"v.io/..."})
	if err != nil {
		return nil, err
	}
	return goBuild(ctx, testName, pkgs)
}

// vanadiumGoCoverage runs Go coverage tests for vanadium projects.
func vanadiumGoCoverage(ctx *tool.Context, testName string, opts ...Opt) (_ *test.Result, e error) {
	// Initialize the test.
	cleanup, err := initTest(ctx, testName, []string{})
	if err != nil {
		return nil, internalTestError{err, "Init"}
	}
	defer collect.Error(func() error { return cleanup() }, &e)

	// Compute coverage for Vanadium Go packages.
	pkgs, err := validateAgainstDefaultPackages(ctx, opts, []string{"v.io/..."})
	if err != nil {
		return nil, err
	}
	return goCoverage(ctx, testName, pkgs)
}

// vanadiumGoGenerate checks that files created by 'go generate' are
// up-to-date.
func vanadiumGoGenerate(ctx *tool.Context, testName string, opts ...Opt) (_ *test.Result, e error) {
	// Initialize the test.
	cleanup, err := initTest(ctx, testName, []string{})
	if err != nil {
		return nil, internalTestError{err, "Init"}
	}
	defer collect.Error(func() error { return cleanup() }, &e)

	pkgs, err := validateAgainstDefaultPackages(ctx, opts, []string{"v.io/..."})
	if err != nil {
		return nil, err
	}
	pkgStr := strings.Join([]string(pkgs), " ")
	fmt.Fprintf(ctx.Stdout(), "NOTE: This test checks that files created by 'go generate' are up-to-date.\nIf it fails, regenerate them using 'v23 go generate %s'.\n", pkgStr)

	// Stash any uncommitted changes and defer functions that undo any
	// changes created by this function and then unstash the original
	// uncommitted changes.
	projects, err := util.LocalProjects(ctx)
	if err != nil {
		return nil, err
	}
	for _, project := range projects {
		if err := ctx.Run().Chdir(project.Path); err != nil {
			return nil, err
		}
		stashed, err := ctx.Git().Stash()
		if err != nil {
			return nil, err
		}
		defer collect.Error(func() error {
			if err := ctx.Run().Chdir(project.Path); err != nil {
				return err
			}
			if err := ctx.Git().Reset("HEAD"); err != nil {
				return err
			}
			if stashed {
				return ctx.Git().StashPop()
			}
			return nil
		}, &e)
	}

	// Check if 'go generate' creates any changes.
	args := append([]string{"go", "generate"}, []string(pkgs)...)
	if err := ctx.Run().Command("v23", args...); err != nil {
		return nil, internalTestError{err, "Go Generate"}
	}
	dirtyFiles := []string{}
	for _, project := range projects {
		files, err := ctx.Git(tool.RootDirOpt(project.Path)).FilesWithUncommittedChanges()
		if err != nil {
			return nil, err
		}
		dirtyFiles = append(dirtyFiles, files...)
	}
	if len(dirtyFiles) != 0 {
		output := strings.Join(dirtyFiles, "\n")
		fmt.Fprintf(ctx.Stdout(), "The following go generated files are not up-to-date:\n%v\n", output)
		// Generate xUnit report.
		suites := []xunit.TestSuite{}
		for _, dirtyFile := range dirtyFiles {
			s := xunit.CreateTestSuiteWithFailure("GoGenerate", dirtyFile, "go generate failure", "Outdated file:\n"+dirtyFile, 0)
			suites = append(suites, *s)
		}
		if err := xunit.CreateReport(ctx, testName, suites); err != nil {
			return nil, err
		}
		return &test.Result{Status: test.Failed}, nil
	}
	return &test.Result{Status: test.Passed}, nil
}

// vanadiumGoRace runs Go data-race tests for vanadium projects.
func vanadiumGoRace(ctx *tool.Context, testName string, opts ...Opt) (_ *test.Result, e error) {
	// Initialize the test.
	cleanup, err := initTest(ctx, testName, []string{})
	if err != nil {
		return nil, internalTestError{err, "Init"}
	}
	defer collect.Error(func() error { return cleanup() }, &e)

	pkgs, err := validateAgainstDefaultPackages(ctx, opts, []string{"v.io/..."})
	if err != nil {
		return nil, err
	}
	partPkgs, err := identifyPackagesToTest(ctx, testName, opts, pkgs)
	if err != nil {
		return nil, err
	}
	exclusions := append(goExclusions, goRaceExclusions...)
	args := argsOpt([]string{"-race"})
	timeout := timeoutOpt("15m")
	suffix := suffixOpt(genTestNameSuffix("GoRace"))
	return goTest(ctx, testName, args, timeout, suffix, exclusionsOpt(exclusions), partPkgs)
}

// identifyPackagesToTest returns a slice of packages to test using the
// following algorithm:
// - The part index is stored in the "P" environment variable. If it is not
//   defined, return all packages.
// - If the part index is found, return the corresponding packages read and
//   processed from the config file. Note that for a test T with N parts, we
//   only specify the packages for the first N-1 parts in the config file. The
//   last part will automatically include all the packages that are not found
//   in the first N-1 parts.
func identifyPackagesToTest(ctx *tool.Context, testName string, opts []Opt, allPkgs []string) (pkgsOpt, error) {
	// Read config file to get the part.
	config, err := util.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	parts := config.TestParts(testName)
	if len(parts) == 0 {
		return pkgsOpt(allPkgs), nil
	}

	// Get part index from optionals.
	index := -1
	for _, opt := range opts {
		switch v := opt.(type) {
		case PartOpt:
			index = int(v)
		}
	}
	if index == -1 {
		return pkgsOpt(allPkgs), nil
	}

	if index == len(parts) {
		// Special handling for getting the packages other than the packages
		// specified in "test-parts".

		// Get packages specified in test-parts.
		existingPartsPkgs := map[string]struct{}{}
		for _, pkg := range parts {
			curPkgs, err := goutil.List(ctx, []string{pkg})
			if err != nil {
				return nil, err
			}
			for _, curPkg := range curPkgs {
				existingPartsPkgs[curPkg] = struct{}{}
			}
		}

		// Get the rest.
		rest := []string{}
		allPkgs, err := goutil.List(ctx, allPkgs)
		if err != nil {
			return nil, err
		}
		for _, pkg := range allPkgs {
			if _, ok := existingPartsPkgs[pkg]; !ok {
				rest = append(rest, pkg)
			}
		}
		return pkgsOpt(rest), nil
	} else if index < len(parts) {
		pkgs, err := goutil.List(ctx, []string{parts[index]})
		if err != nil {
			return nil, err
		}
		return pkgsOpt(pkgs), nil
	}
	return nil, fmt.Errorf("invalid part index: %d/%d", index, len(parts)-1)
}

// vanadiumIntegrationTest runs integration tests for Vanadium
// projects.
func vanadiumIntegrationTest(ctx *tool.Context, testName string, opts ...Opt) (_ *test.Result, e error) {
	// Initialize the test.
	cleanup, err := initTest(ctx, testName, []string{})
	if err != nil {
		return nil, internalTestError{err, "Init"}
	}
	defer collect.Error(func() error { return cleanup() }, &e)

	pkgs, err := validateAgainstDefaultPackages(ctx, opts, []string{"v.io/..."})
	if err != nil {
		return nil, err
	}
	suffix := suffixOpt(genTestNameSuffix("V23Test"))
	args := argsOpt([]string{"-run", "^TestV23"})
	nonTestArgs := nonTestArgsOpt([]string{"-v23.tests"})
	matcher := funcMatcherOpt{&matchV23TestFunc{}}
	env := ctx.Env()
	env["V23_BIN_DIR"] = binDirPath()
	newCtx := ctx.Clone(tool.ContextOpts{Env: env})
	return goTest(newCtx, testName, suffix, args, getNumWorkersOpt(opts), nonTestArgs, matcher, pkgs)
}

type binSet map[string]struct{}

// regressionBinSets are sets of binaries to test against older or newer binaries.
// These correspond to typical upgrade scenarios.  For example it is somewhat
// common to upgrade an agent against old binaries, or to upgrade other binaries
// while keeping an agent old, therefore it's nice to see how just a new/old
// agent does against other binaries.
var regressionBinSets = map[string]binSet{
	"agentonly": {
		"agentd": struct{}{},
	},
	"agentdevice": {
		"agentd":  struct{}{},
		"deviced": struct{}{},
	},
	"prodservices": {
		"agentd":       struct{}{},
		"deviced":      struct{}{},
		"applicationd": struct{}{},
		"binaryd":      struct{}{},
		"identityd":    struct{}{},
		"proxyd":       struct{}{},
		"mounttabled":  struct{}{},
	},
}

// regressionDirection determines if the regression tests uses
// new binaries for the selected binSet and old binaries for
// everything else, or the opposite.
type regressionDirection string

const (
	newBinSet = regressionDirection("new")
	oldBinSet = regressionDirection("old")
)

// vanadiumRegressionTest runs integration tests for Vanadium projects
// using different versions of Vanadium binaries.
func vanadiumRegressionTest(ctx *tool.Context, testName string, opts ...Opt) (_ *test.Result, e error) {
	var againstDate time.Time
	if dateStr := os.Getenv("V23_REGTEST_DATE"); dateStr != "" {
		if againstDate, e = time.Parse("2006-01-02", dateStr); e != nil {
			return nil, fmt.Errorf("time.Parse(%q) failed: %v", dateStr, e)
		}
	} else {
		var days uint64 = 1
		if daysStr := os.Getenv("V23_REGTEST_DAYS"); daysStr != "" {
			if days, e = strconv.ParseUint(daysStr, 10, 32); e != nil {
				return nil, fmt.Errorf("ParseUint(%q) failed: %v", daysStr, e)
			}
		}
		againstDate = time.Now().AddDate(0, 0, -int(days))
	}

	var set map[string]struct{}
	if binSetStr := os.Getenv("V23_REGTEST_BINSET"); binSetStr != "" {
		if set = regressionBinSets[binSetStr]; set == nil {
			return nil, fmt.Errorf("specified binset %q is not valid", binSetStr)
		}
	} else if binariesStr := os.Getenv("V23_REGTEST_BINARIES"); binariesStr != "" {
		set = binSet{}
		for _, name := range strings.Split(binariesStr, ",") {
			set[name] = struct{}{}
		}
	} else {
		set = regressionBinSets["prodservices"]
	}

	var directions []regressionDirection
	if directionStr := os.Getenv("V23_REGTEST_DIR"); directionStr != "" {
		dir := regressionDirection(directionStr)
		if dir != newBinSet && dir != oldBinSet {
			return nil, fmt.Errorf("specified direction %q is not valid", directionStr)
		}
		directions = []regressionDirection{dir}
	} else {
		directions = []regressionDirection{oldBinSet, newBinSet}
	}

	// By default we only run TestV23Hello.* because there are often
	// changes to flags command line interfaces that often break other
	// tests.  In the future we may be more strict about compatibility
	// for command line utilities and add more tests here.
	var testsToRun = "^TestV23Hello.*"
	if testsStr := os.Getenv("V23_REGTEST_TESTS"); testsStr != "" {
		testsToRun = testsStr
	}

	// Initialize the test.
	cleanup, err := initTest(ctx, testName, []string{})
	if err != nil {
		return nil, internalTestError{err, "Init"}
	}
	defer collect.Error(func() error { return cleanup() }, &e)

	pkgs, err := validateAgainstDefaultPackages(ctx, opts, []string{"v.io/..."})
	if err != nil {
		return nil, err
	}
	suffix := suffixOpt(genTestNameSuffix("V23Test"))
	args := argsOpt([]string{"-run", testsToRun})
	nonTestArgs := nonTestArgsOpt([]string{"-v23.tests"})
	matcher := funcMatcherOpt{&matchV23TestFunc{}}

	// Build all v.io binaries.  We are going to check the binaries at head
	// against those from a previous date.
	if err := ctx.Run().Command("v23", "go", "install", "v.io/..."); err != nil {
		return nil, internalTestError{err, "Install"}
	}
	root, err := util.V23Root()
	if err != nil {
		return nil, err
	}
	newBinDir := filepath.Join(root, "release", "go", "bin")

	// Now download all the binaries for a previous date.
	oldBinDir, err := downloadVanadiumBinaries(ctx, againstDate)
	if err == noSnapshotErr {
		return &test.Result{Status: test.Skipped}, nil
	}
	if err != nil {
		return nil, err
	}

	outBinDir := filepath.Join(regTestBinDirPath(), "bin")
	env := ctx.Env()
	env["V23_BIN_DIR"] = outBinDir
	env["V23_REGTEST_DATE"] = againstDate.Format("2006-01-02")
	newCtx := ctx.Clone(tool.ContextOpts{Env: env})
	// Now we run tests with various sets of new/old binaries.
	for _, dir := range directions {
		in1, in2 := oldBinDir, newBinDir
		if dir == newBinSet {
			in1, in2 = in2, in1
		}
		if err := prepareVanadiumBinaries(ctx, in1, in2, outBinDir, set); err != nil {
			return nil, err
		}
		result, err := goTest(newCtx, testName, suffix, args, getNumWorkersOpt(opts), nonTestArgs, matcher, pkgs)
		if err != nil || (result.Status != test.Passed && result.Status != test.Skipped) {
			return result, err
		}
	}
	return &test.Result{Status: test.Passed}, nil
}

// noSnapshotErr is returned from downloadVanadiumBinaries when there were no
// binaries for the given date.
var noSnapshotErr = fmt.Errorf("no snapshots for specified date.")

func downloadVanadiumBinaries(ctx *tool.Context, date time.Time) (binDir string, e error) {
	dateStr := date.Format("2006-01-02")
	binDir = filepath.Join(regTestBinDirPath(), dateStr)
	_, err := os.Stat(binDir)
	if err == nil {
		return binDir, nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("Stat() failed: %v", err)
	}

	tmpDir, err := ctx.Run().TempDir("", "")
	if err != nil {
		return "", err
	}
	defer collect.Error(func() error { return ctx.Run().RemoveAll(tmpDir) }, &e)
	vbinaryBin := filepath.Join(tmpDir, "vbinary")
	if err := ctx.Run().Command("v23", "go", "build", "-o", vbinaryBin, "v.io/x/devtools/vbinary"); err != nil {
		return "", err
	}
	if err := ctx.Run().Command(vbinaryBin,
		"-date-prefix", dateStr,
		"-key-file", os.Getenv("V23_KEY_FILE"),
		"download",
		"-output-dir", binDir); err != nil {
		exiterr, ok := err.(*exec.ExitError)
		if !ok {
			return "", err
		}
		status, ok := exiterr.Sys().(syscall.WaitStatus)
		if !ok {
			return "", err
		}
		if status.ExitStatus() == util.NoSnapshotExitCode {
			return "", noSnapshotErr
		}
	}
	return binDir, nil
}

// prepareRegressionBinaries assembles binaries into the directory out by taking
// binaries from in1 and in2.  Binaries in the list take1 will be taken
// from 1, other will be taken from 2.
func prepareVanadiumBinaries(ctx *tool.Context, in1, in2, out string, take1 binSet) error {
	if err := ctx.Run().RemoveAll(out); err != nil {
		return err
	}
	if err := ctx.Run().MkdirAll(out, os.FileMode(0755)); err != nil {
		return err
	}

	binaries := make(map[string]string)

	// First take everything from in2.
	fileInfos, err := ioutil.ReadDir(in2)
	if err != nil {
		return fmt.Errorf("ReadDir(%v) failed: %v", in2, err)
	}
	for _, fileInfo := range fileInfos {
		name := fileInfo.Name()
		binaries[name] = filepath.Join(in2, name)
	}

	// Now take things from in1 if they are in take1, or were missing from in2.
	fileInfos, err = ioutil.ReadDir(in1)
	if err != nil {
		return fmt.Errorf("ReadDir(%v) failed: %v", in1, err)
	}
	for _, fileInfo := range fileInfos {
		name := fileInfo.Name()
		_, inSet := take1[name]
		if inSet || binaries[name] == "" {
			binaries[name] = filepath.Join(in1, name)
		}
	}

	// We want to print some info in sorted order for easy reading.
	sortedBinaries := make([]string, 0, len(binaries))
	for name := range binaries {
		sortedBinaries = append(sortedBinaries, name)
	}
	sort.Strings(sortedBinaries)

	// We go through the sorted list twice.  The first time we print
	// the hold-out binaries, the second time the rest.  This just
	// makes it easier to read the debug output.
	for _, holdout := range []bool{true, false} {
		for _, name := range sortedBinaries {
			if _, ok := take1[name]; ok != holdout {
				continue
			}
			src := binaries[name]
			dst := filepath.Join(out, name)
			if err := ctx.Run().Symlink(src, dst); err != nil {
				return err
			}
			fmt.Fprintf(ctx.Stdout(), "using %s from %s\n", name, src)
		}
	}

	return nil
}

func genTestNameSuffix(baseSuffix string) string {
	suffixParts := []string{}
	suffixParts = append(suffixParts, runtime.GOOS)
	arch := os.Getenv("GOARCH")
	if arch == "" {
		var err error
		arch, err = host.Arch()
		if err != nil {
			arch = "amd64"
		}
	}
	suffixParts = append(suffixParts, arch)
	suffix := strings.Join(suffixParts, ",")

	if baseSuffix == "" {
		return fmt.Sprintf("[%s]", suffix)
	}
	return fmt.Sprintf("[%s - %s]", baseSuffix, suffix)
}

// vanadiumGoTest runs Go tests for vanadium projects.
func vanadiumGoTest(ctx *tool.Context, testName string, opts ...Opt) (_ *test.Result, e error) {
	// Initialize the test.
	cleanup, err := initTest(ctx, testName, []string{})
	if err != nil {
		return nil, internalTestError{err, "Init"}
	}
	defer collect.Error(func() error { return cleanup() }, &e)

	// Test the Vanadium Go packages.
	pkgs, err := validateAgainstDefaultPackages(ctx, opts, []string{"v.io/..."})
	if err != nil {
		return nil, err
	}
	args := argsOpt([]string{})
	suffix := suffixOpt(genTestNameSuffix("GoTest"))
	return goTest(ctx, testName, suffix, exclusionsOpt(goExclusions), getNumWorkersOpt(opts), pkgs, args)
}
