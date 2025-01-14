package config

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAlertsSettings(t *testing.T) {
	type testCaseT struct {
		conf AlertsSettings
		err  error
	}

	testCases := []testCaseT{
		{
			conf: AlertsSettings{
				Range: "7d",
			},
		},
		{
			conf: AlertsSettings{
				Step: "7d",
			},
		},
		{
			conf: AlertsSettings{
				Resolve: "7d",
			},
		},
		{
			conf: AlertsSettings{
				Range:   "foo",
				Step:    "1m",
				Resolve: "5m",
			},
			err: errors.New(`not a valid duration string: "foo"`),
		},
		{
			conf: AlertsSettings{
				Range:   "7d",
				Step:    "foo",
				Resolve: "5m",
			},
			err: errors.New(`not a valid duration string: "foo"`),
		},
		{
			conf: AlertsSettings{
				Range:   "7d",
				Step:    "1m",
				Resolve: "foo",
			},
			err: errors.New(`not a valid duration string: "foo"`),
		},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%v", tc.conf), func(t *testing.T) {
			assert := assert.New(t)
			err := tc.conf.validate()
			if err == nil || tc.err == nil {
				assert.Equal(err, tc.err)
			} else {
				assert.EqualError(err, tc.err.Error())
			}
		})
	}
}
