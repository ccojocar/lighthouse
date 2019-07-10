/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package trigger

import (
	"reflect"
	"testing"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/sets"
	clienttesting "k8s.io/client-go/testing"

	"github.com/jenkins-x/lighthouse/pkg/prow/client/clientset/versioned/fake"
	"github.com/jenkins-x/lighthouse/pkg/prow/config"
	"github.com/jenkins-x/lighthouse/pkg/prow/fakegithub"
	"github.com/jenkins-x/lighthouse/pkg/prow/github"
	"github.com/jenkins-x/lighthouse/pkg/prow/plugins"
	prowapi "k8s.io/test-infra/prow/apis/plumberJobs/v1"
)

func TestHelpProvider(t *testing.T) {
	cases := []struct {
		name         string
		config       *plugins.Configuration
		enabledRepos []string
		err          bool
	}{
		{
			name:         "Empty config",
			config:       &plugins.Configuration{},
			enabledRepos: []string{"org1", "org2/repo"},
		},
		{
			name:         "Overlapping org and org/repo",
			config:       &plugins.Configuration{},
			enabledRepos: []string{"org2", "org2/repo"},
		},
		{
			name:         "Invalid enabledRepos",
			config:       &plugins.Configuration{},
			enabledRepos: []string{"org1", "org2/repo/extra"},
			err:          true,
		},
		{
			name: "All configs enabled",
			config: &plugins.Configuration{
				Triggers: []plugins.Trigger{
					{
						Repos:          []string{"org2"},
						TrustedOrg:     "org2",
						JoinOrgURL:     "https://join.me",
						OnlyOrgMembers: true,
						IgnoreOkToTest: true,
					},
				},
			},
			enabledRepos: []string{"org1", "org2/repo"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := helpProvider(c.config, c.enabledRepos)
			if err != nil && !c.err {
				t.Fatalf("helpProvider error: %v", err)
			}
		})
	}
}

func TestRunAndSkipJobs(t *testing.T) {
	var testCases = []struct {
		name string

		requestedJobs        []config.Presubmit
		skippedJobs          []config.Presubmit
		elideSkippedContexts bool
		jobCreationErrs      sets.String // job names which fail creation

		expectedJobs     sets.String // by name
		expectedStatuses []*scm.Status
		expectedErr      bool
	}{
		{
			name: "nothing requested means nothing done",
		},
		{
			name: "all requested jobs get run",
			requestedJobs: []config.Presubmit{{
				JobBase: config.JobBase{
					Name: "first",
				},
				Reporter: config.Reporter{Context: "first-context"},
			}, {
				JobBase: config.JobBase{
					Name: "second",
				},
				Reporter: config.Reporter{Context: "second-context"},
			}},
			expectedJobs: sets.NewString("first", "second"),
		},
		{
			name: "failure on job creation bubbles up but doesn't stop others from starting",
			requestedJobs: []config.Presubmit{{
				JobBase: config.JobBase{
					Name: "first",
				},
				Reporter: config.Reporter{Context: "first-context"},
			}, {
				JobBase: config.JobBase{
					Name: "second",
				},
				Reporter: config.Reporter{Context: "second-context"},
			}},
			jobCreationErrs: sets.NewString("first"),
			expectedJobs:    sets.NewString("second"),
			expectedErr:     true,
		},
		{
			name: "all skipped jobs get skipped",
			skippedJobs: []config.Presubmit{{
				JobBase: config.JobBase{
					Name: "first",
				},
				Reporter: config.Reporter{Context: "first-context"},
			}, {
				JobBase: config.JobBase{
					Name: "second",
				},
				Reporter: config.Reporter{Context: "second-context"},
			}},
			expectedStatuses: []*scm.Status{{
				State:       scm.StateSuccess,
				Context:     "first-context",
				Description: "Skipped.",
			}, {
				State:       scm.StateSuccess,
				Context:     "second-context",
				Description: "Skipped.",
			}},
		},
		{
			name: "all skipped jobs get ignored if skipped statuses are elided",
			skippedJobs: []config.Presubmit{{
				JobBase: config.JobBase{
					Name: "first",
				},
				Reporter: config.Reporter{Context: "first-context"},
			}, {
				JobBase: config.JobBase{
					Name: "second",
				},
				Reporter: config.Reporter{Context: "second-context"},
			}},
			elideSkippedContexts: true,
		},
		{
			name: "skipped jobs with skip report get ignored",
			skippedJobs: []config.Presubmit{{
				JobBase: config.JobBase{
					Name: "first",
				},
				Reporter: config.Reporter{Context: "first-context"},
			}, {
				JobBase: config.JobBase{
					Name: "second",
				},
				Reporter: config.Reporter{Context: "second-context", SkipReport: true},
			}},
			expectedStatuses: []*scm.Status{{
				State:       scm.StateSuccess,
				Context:     "first-context",
				Description: "Skipped.",
			}},
		},
		{
			name: "overlap between jobs callErrors and has no external action",
			requestedJobs: []config.Presubmit{{
				JobBase: config.JobBase{
					Name: "first",
				},
				Reporter: config.Reporter{Context: "first-context"},
			}, {
				JobBase: config.JobBase{
					Name: "second",
				},
				Reporter: config.Reporter{Context: "second-context"},
			}},
			skippedJobs: []config.Presubmit{{
				JobBase: config.JobBase{
					Name: "first",
				},
				Reporter: config.Reporter{Context: "first-context"},
			}},
			expectedErr: true,
		},
		{
			name: "disjoint sets of jobs get triggered and skipped correctly",
			requestedJobs: []config.Presubmit{{
				JobBase: config.JobBase{
					Name: "first",
				},
				Reporter: config.Reporter{Context: "first-context"},
			}, {
				JobBase: config.JobBase{
					Name: "second",
				},
				Reporter: config.Reporter{Context: "second-context"},
			}},
			skippedJobs: []config.Presubmit{{
				JobBase: config.JobBase{
					Name: "third",
				},
				Reporter: config.Reporter{Context: "third-context"},
			}, {
				JobBase: config.JobBase{
					Name: "fourth",
				},
				Reporter: config.Reporter{Context: "fourth-context"},
			}},
			expectedJobs: sets.NewString("first", "second"),
			expectedStatuses: []*scm.Status{{
				State:       scm.StateSuccess,
				Context:     "third-context",
				Description: "Skipped.",
			}, {
				State:       scm.StateSuccess,
				Context:     "fourth-context",
				Description: "Skipped.",
			}},
		},
		{
			name: "disjoint sets of jobs get triggered and skipped correctly, even if one creation fails",
			requestedJobs: []config.Presubmit{{
				JobBase: config.JobBase{
					Name: "first",
				},
				Reporter: config.Reporter{Context: "first-context"},
			}, {
				JobBase: config.JobBase{
					Name: "second",
				},
				Reporter: config.Reporter{Context: "second-context"},
			}},
			skippedJobs: []config.Presubmit{{
				JobBase: config.JobBase{
					Name: "third",
				},
				Reporter: config.Reporter{Context: "third-context"},
			}, {
				JobBase: config.JobBase{
					Name: "fourth",
				},
				Reporter: config.Reporter{Context: "fourth-context"},
			}},
			jobCreationErrs: sets.NewString("first"),
			expectedJobs:    sets.NewString("second"),
			expectedStatuses: []*scm.Status{{
				State:       scm.StateSuccess,
				Context:     "third-context",
				Description: "Skipped.",
			}, {
				State:       scm.StateSuccess,
				Context:     "fourth-context",
				Description: "Skipped.",
			}},
			expectedErr: true,
		},
	}

	pr := &scm.PullRequest{
		Base: scm.PullRequestBranch{
			Repo: scm.Repository{
				Owner: scm.User{
					Login: "org",
				},
				Name: "repo",
			},
			Ref: "branch",
		},
		Head: scm.PullRequestBranch{
			Sha: "foobar1",
		},
	}

	for _, testCase := range testCases {
		fakeGitHubClient := fakegithub.FakeClient{}
		fakePlumberClient := fake.NewSimpleClientset()
		fakePlumberClient.PrependReactor("*", "*", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			switch action := action.(type) {
			case clienttesting.CreateActionImpl:
				plumberJob, ok := action.Object.(*builder.PlumberJob)
				if !ok {
					return false, nil, nil
				}
				if testCase.jobCreationErrs.Has(plumberJob.Spec.Job) {
					return true, action.Object, errors.New("failed to create job")
				}
			}
			return false, nil, nil
		})
		client := Client{
			GitHubClient:  &fakeGitHubClient,
			PlumberClient: fakePlumberClient.ProwV1().PlumberJobs("plumberJobs"),
			Logger:        logrus.WithField("testcase", testCase.name),
		}

		err := RunAndSkipJobs(client, pr, testCase.requestedJobs, testCase.skippedJobs, "event-guid", testCase.elideSkippedContexts)
		if err == nil && testCase.expectedErr {
			t.Errorf("%s: expected an error but got none", testCase.name)
		}
		if err != nil && !testCase.expectedErr {
			t.Errorf("%s: expected no error but got one: %v", testCase.name, err)
		}

		if actual, expected := fakeGitHubClient.CreatedStatuses[pr.Sha], testCase.expectedStatuses; !reflect.DeepEqual(actual, expected) {
			t.Errorf("%s: created incorrect statuses: %s", testCase.name, diff.ObjectReflectDiff(actual, expected))
		}

		observedCreatedPlumberJobs := sets.NewString()
		existingPlumberJobs, err := fakePlumberClient.ProwV1().PlumberJobs("plumberJobs").List(metav1.ListOptions{})
		if err != nil {
			t.Errorf("%s: could not list current state of prow jobs: %v", testCase.name, err)
			continue
		}
		for _, job := range existingPlumberJobs.Items {
			observedCreatedPlumberJobs.Insert(job.Spec.Job)
		}

		if missing := testCase.expectedJobs.Difference(observedCreatedPlumberJobs); missing.Len() > 0 {
			t.Errorf("%s: didn't create all expected PlumberJobs, missing: %s", testCase.name, missing.List())
		}
		if extra := observedCreatedPlumberJobs.Difference(testCase.expectedJobs); extra.Len() > 0 {
			t.Errorf("%s: created unexpected PlumberJobs: %s", testCase.name, extra.List())
		}
	}
}

func TestRunRequested(t *testing.T) {
	var testCases = []struct {
		name string

		requestedJobs   []config.Presubmit
		jobCreationErrs sets.String // job names which fail creation

		expectedJobs sets.String // by name
		expectedErr  bool
	}{
		{
			name: "nothing requested means nothing done",
		},
		{
			name: "all requested jobs get run",
			requestedJobs: []config.Presubmit{{
				JobBase: config.JobBase{
					Name: "first",
				},
				Reporter: config.Reporter{Context: "first-context"},
			}, {
				JobBase: config.JobBase{
					Name: "second",
				},
				Reporter: config.Reporter{Context: "second-context"},
			}},
			expectedJobs: sets.NewString("first", "second"),
		},
		{
			name: "failure on job creation bubbles up but doesn't stop others from starting",
			requestedJobs: []config.Presubmit{{
				JobBase: config.JobBase{
					Name: "first",
				},
				Reporter: config.Reporter{Context: "first-context"},
			}, {
				JobBase: config.JobBase{
					Name: "second",
				},
				Reporter: config.Reporter{Context: "second-context"},
			}},
			jobCreationErrs: sets.NewString("first"),
			expectedJobs:    sets.NewString("second"),
			expectedErr:     true,
		},
	}

	pr := &scm.PullRequest{
		Base: scm.PullRequestBranch{
			Repo: scm.Repository{
				Owner: scm.User{
					Login: "org",
				},
				Name: "repo",
			},
			Ref: "branch",
		},
		Head: scm.PullRequestBranch{
			Sha: "foobar1",
		},
	}

	for _, testCase := range testCases {
		fakeGitHubClient := fakegithub.FakeClient{}
		fakePlumberClient := fake.NewSimpleClientset()
		fakePlumberClient.PrependReactor("*", "*", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			switch action := action.(type) {
			case clienttesting.CreateActionImpl:
				plumberJob, ok := action.Object.(*builder.PlumberJob)
				if !ok {
					return false, nil, nil
				}
				if testCase.jobCreationErrs.Has(plumberJob.Spec.Job) {
					return true, action.Object, errors.New("failed to create job")
				}
			}
			return false, nil, nil
		})
		client := Client{
			GitHubClient:  &fakeGitHubClient,
			PlumberClient: fakePlumberClient.ProwV1().PlumberJobs("plumberJobs"),
			Logger:        logrus.WithField("testcase", testCase.name),
		}

		err := runRequested(client, pr, testCase.requestedJobs, "event-guid")
		if err == nil && testCase.expectedErr {
			t.Errorf("%s: expected an error but got none", testCase.name)
		}
		if err != nil && !testCase.expectedErr {
			t.Errorf("%s: expected no error but got one: %v", testCase.name, err)
		}

		observedCreatedPlumberJobs := sets.NewString()
		existingPlumberJobs, err := fakePlumberClient.ProwV1().PlumberJobs("plumberJobs").List(metav1.ListOptions{})
		if err != nil {
			t.Errorf("%s: could not list current state of prow jobs: %v", testCase.name, err)
			continue
		}
		for _, job := range existingPlumberJobs.Items {
			observedCreatedPlumberJobs.Insert(job.Spec.Job)
		}

		if missing := testCase.expectedJobs.Difference(observedCreatedPlumberJobs); missing.Len() > 0 {
			t.Errorf("%s: didn't create all expected PlumberJobs, missing: %s", testCase.name, missing.List())
		}
		if extra := observedCreatedPlumberJobs.Difference(testCase.expectedJobs); extra.Len() > 0 {
			t.Errorf("%s: created unexpected PlumberJobs: %s", testCase.name, extra.List())
		}
	}
}

func TestValidateContextOverlap(t *testing.T) {
	var testCases = []struct {
		name          string
		toRun, toSkip []config.Presubmit
		expectedErr   bool
	}{
		{
			name:   "empty inputs mean no error",
			toRun:  []config.Presubmit{},
			toSkip: []config.Presubmit{},
		},
		{
			name:   "disjoint sets mean no error",
			toRun:  []config.Presubmit{{Reporter: config.Reporter{Context: "foo"}}},
			toSkip: []config.Presubmit{{Reporter: config.Reporter{Context: "bar"}}},
		},
		{
			name:   "complex disjoint sets mean no error",
			toRun:  []config.Presubmit{{Reporter: config.Reporter{Context: "foo"}}, {Reporter: config.Reporter{Context: "otherfoo"}}},
			toSkip: []config.Presubmit{{Reporter: config.Reporter{Context: "bar"}}, {Reporter: config.Reporter{Context: "otherbar"}}},
		},
		{
			name:        "overlapping sets error",
			toRun:       []config.Presubmit{{Reporter: config.Reporter{Context: "foo"}}, {Reporter: config.Reporter{Context: "otherfoo"}}},
			toSkip:      []config.Presubmit{{Reporter: config.Reporter{Context: "bar"}}, {Reporter: config.Reporter{Context: "otherfoo"}}},
			expectedErr: true,
		},
		{
			name:        "identical sets error",
			toRun:       []config.Presubmit{{Reporter: config.Reporter{Context: "foo"}}, {Reporter: config.Reporter{Context: "otherfoo"}}},
			toSkip:      []config.Presubmit{{Reporter: config.Reporter{Context: "foo"}}, {Reporter: config.Reporter{Context: "otherfoo"}}},
			expectedErr: true,
		},
		{
			name:        "superset callErrors",
			toRun:       []config.Presubmit{{Reporter: config.Reporter{Context: "foo"}}, {Reporter: config.Reporter{Context: "otherfoo"}}},
			toSkip:      []config.Presubmit{{Reporter: config.Reporter{Context: "foo"}}, {Reporter: config.Reporter{Context: "otherfoo"}}, {Reporter: config.Reporter{Context: "thirdfoo"}}},
			expectedErr: true,
		},
	}

	for _, testCase := range testCases {
		validateErr := validateContextOverlap(testCase.toRun, testCase.toSkip)
		if validateErr == nil && testCase.expectedErr {
			t.Errorf("%s: expected an error but got none", testCase.name)
		}
		if validateErr != nil && !testCase.expectedErr {
			t.Errorf("%s: expected no error but got one: %v", testCase.name, validateErr)
		}
	}
}