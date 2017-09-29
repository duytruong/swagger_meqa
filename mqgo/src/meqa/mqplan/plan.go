package mqplan

import (
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	resty "gopkg.in/resty.v0"
	"gopkg.in/yaml.v2"

	"meqa/mqswag"
	"meqa/mqutil"
)

const (
	MeqaInit = "meqa_init"
)

type TestParams struct {
	QueryParams  map[string]interface{} `yaml:"queryParams,omitempty"`
	FormParams   map[string]interface{} `yaml:"formParams,omitempty"`
	PathParams   map[string]interface{} `yaml:"pathParams,omitempty"`
	HeaderParams map[string]interface{} `yaml:"headerParams,omitempty"`
	BodyParams   interface{}            `yaml:"bodyParams,omitempty"`
}

// Copy the parameters from src. If there is a conflict dst will be overwritten.
func (dst *TestParams) Copy(src *TestParams) {
	dst.QueryParams = mqutil.MapCombine(dst.QueryParams, src.QueryParams)
	dst.FormParams = mqutil.MapCombine(dst.FormParams, src.FormParams)
	dst.PathParams = mqutil.MapCombine(dst.PathParams, src.PathParams)
	dst.HeaderParams = mqutil.MapCombine(dst.HeaderParams, src.HeaderParams)

	if caseMap, caseIsMap := dst.BodyParams.(map[string]interface{}); caseIsMap {
		if testMap, testIsMap := src.BodyParams.(map[string]interface{}); testIsMap {
			dst.BodyParams = mqutil.MapCombine(caseMap, testMap)
			// for map, just combine and return
			return
		}
	}
	dst.BodyParams = src.BodyParams
}

// Add the parameters from src. If there is a conflict the dst original value will be kept.
func (dst *TestParams) Add(src *TestParams) {
	dst.QueryParams = mqutil.MapAdd(dst.QueryParams, src.QueryParams)
	dst.FormParams = mqutil.MapAdd(dst.FormParams, src.FormParams)
	dst.PathParams = mqutil.MapAdd(dst.PathParams, src.PathParams)
	dst.HeaderParams = mqutil.MapAdd(dst.HeaderParams, src.HeaderParams)

	if caseMap, caseIsMap := dst.BodyParams.(map[string]interface{}); caseIsMap {
		if testMap, testIsMap := src.BodyParams.(map[string]interface{}); testIsMap {
			dst.BodyParams = mqutil.MapAdd(caseMap, testMap)
			// for map, just combine and return
			return
		}
	}

	if dst.BodyParams == nil {
		dst.BodyParams = src.BodyParams
	}
}

type TestSuite struct {
	Tests []*Test
	Name  string

	// test suite parameters
	TestParams `yaml:",inline,omitempty" json:",inline,omitempty"`
	Strict     bool

	// Authentication
	Username string
	Password string
	ApiToken string

	plan *TestPlan
	db   *mqswag.DB // objects generated/obtained as part of this suite
}

func CreateTestSuite(name string, tests []*Test, plan *TestPlan) *TestSuite {
	c := TestSuite{}
	c.Name = name
	c.Tests = tests
	(&c.TestParams).Copy(&plan.TestParams)
	c.Strict = plan.Strict

	c.Username = plan.Username
	c.Password = plan.Password
	c.ApiToken = plan.ApiToken

	c.plan = plan
	return &c
}

// Represents all the test suites in the DSL.
type TestPlan struct {
	SuiteMap  map[string](*TestSuite)
	SuiteList [](*TestSuite)
	db        *mqswag.DB
	swagger   *mqswag.Swagger

	// global parameters
	TestParams `yaml:",inline,omitempty" json:",inline,omitempty"`
	Strict     bool

	// Authentication
	Username string
	Password string
	ApiToken string

	// Run result.
	resultList []*Test
}

// Add a new TestSuite, returns whether the Case is successfully added.
func (plan *TestPlan) Add(testSuite *TestSuite) error {
	if _, exist := plan.SuiteMap[testSuite.Name]; exist {
		str := fmt.Sprintf("Duplicate name %s found in test plan", testSuite.Name)
		mqutil.Logger.Println(str)
		return errors.New(str)
	}
	plan.SuiteMap[testSuite.Name] = testSuite
	plan.SuiteList = append(plan.SuiteList, testSuite)
	return nil
}

func (plan *TestPlan) AddFromString(data string) error {
	var caseMap map[string]([]*Test)
	err := yaml.Unmarshal([]byte(data), &caseMap)
	if err != nil {
		mqutil.Logger.Printf("The following is not a valud TestSuite:\n%s", data)
		return err
	}

	for caseName, testList := range caseMap {
		if caseName == MeqaInit {
			// global parameters
			for _, t := range testList {
				t.Init(nil)
				(&plan.TestParams).Copy(&t.TestParams)
				plan.Strict = t.Strict
			}

			continue
		}
		testSuite := CreateTestSuite(caseName, testList, plan)
		for _, t := range testList {
			t.Init(testSuite)
		}
		err = plan.Add(testSuite)
		if err != nil {
			return err
		}
	}
	return nil
}

func (plan *TestPlan) InitFromFile(path string, db *mqswag.DB) error {
	plan.Init(db.Swagger, db)

	data, err := ioutil.ReadFile(path)
	if err != nil {
		mqutil.Logger.Printf("Can't open the following file: %s", path)
		mqutil.Logger.Println(err.Error())
		return err
	}
	chunks := strings.Split(string(data), "---")
	for _, chunk := range chunks {
		plan.AddFromString(chunk)
	}
	return nil
}

func (plan *TestPlan) DumpToFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	for _, testSuite := range plan.SuiteList {
		count, err := f.WriteString("\n---\n")
		if err != nil {
			return err
		}
		testMap := map[string]interface{}{testSuite.Name: testSuite.Tests}
		caseBytes, err := yaml.Marshal(testMap)
		if err != nil {
			return err
		}
		count, err = f.Write(caseBytes)
		if count != len(caseBytes) || err != nil {
			panic("writing test suite failed")
		}
	}
	return nil
}

func (plan *TestPlan) WriteResultToFile(path string) error {
	// We create a new test plan that just contain all the tests in one test suite.
	p := &TestPlan{}
	tc := &TestSuite{}
	// Test case name is the current time.
	tc.Name = time.Now().Format(time.RFC3339)
	p.SuiteMap = map[string]*TestSuite{tc.Name: tc}
	p.SuiteList = append(p.SuiteList, tc)

	for _, test := range plan.resultList {
		tc.Tests = append(tc.Tests, test)
	}
	return p.DumpToFile(path)
}

func (plan *TestPlan) Init(swagger *mqswag.Swagger, db *mqswag.DB) {
	plan.db = db
	plan.swagger = swagger
	plan.SuiteMap = make(map[string]*TestSuite)
	plan.SuiteList = nil
	plan.resultList = nil
}

// Run a named TestSuite in the test plan.
func (plan *TestPlan) Run(name string, parentTest *Test) error {
	tc, ok := plan.SuiteMap[name]
	if !ok || len(tc.Tests) == 0 {
		str := fmt.Sprintf("The following test suite is not found: %s", name)
		mqutil.Logger.Println(str)
		return errors.New(str)
	}
	tc.db = plan.db.CloneSchema()
	defer func() {
		tc.db = nil
	}()

	for _, test := range tc.Tests {
		if len(test.Ref) != 0 {
			test.Strict = tc.Strict
			err := plan.Run(test.Ref, test)
			if err != nil {
				return err
			}
			continue
		}

		if test.Name == MeqaInit {
			// Apply the parameters to the test suite.
			(&tc.TestParams).Copy(&test.TestParams)
			tc.Strict = test.Strict
			continue
		}

		dup := test.Duplicate()
		dup.Strict = tc.Strict
		if parentTest != nil {
			dup.CopyParent(parentTest)
		}
		dup.ResolveHistoryParameters(&History)
		History.Append(dup)
		if parentTest != nil {
			dup.Name = parentTest.Name // always inherit the name
		}
		err := dup.Run(tc)
		if err != nil {
			dup.err = err
			return err
		}
		plan.resultList = append(plan.resultList, dup)
	}
	return nil
}

// The current global TestPlan
var Current TestPlan

// TestHistory records the execution result of all the tests
type TestHistory struct {
	tests []*Test
	mutex sync.Mutex
}

// GetTest gets a test by its name
func (h *TestHistory) GetTest(name string) *Test {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	for i := len(h.tests) - 1; i >= 0; i-- {
		if h.tests[i].Name == name {
			return h.tests[i]
		}
	}
	return nil
}
func (h *TestHistory) Append(t *Test) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.tests = append(h.tests, t)
}

var History TestHistory

func init() {
	rand.Seed(int64(time.Now().Second()))
	resty.SetRedirectPolicy(resty.FlexibleRedirectPolicy(15))
}
