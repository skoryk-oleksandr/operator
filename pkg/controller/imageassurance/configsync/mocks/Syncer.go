// Code generated by mockery v2.14.0. DO NOT EDIT.

package mocks

import mock "github.com/stretchr/testify/mock"

// Syncer is an autogenerated mock type for the Syncer type
type Syncer struct {
	mock.Mock
}

// Error provides a mock function with given fields:
func (_m *Syncer) Error() error {
	ret := _m.Called()

	var r0 error
	if rf, ok := ret.Get(0).(func() error); ok {
		r0 = rf()
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// StartPeriodicSync provides a mock function with given fields:
func (_m *Syncer) StartPeriodicSync() {
	_m.Called()
}

type mockConstructorTestingTNewSyncer interface {
	mock.TestingT
	Cleanup(func())
}

// NewSyncer creates a new instance of Syncer. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
func NewSyncer(t mockConstructorTestingTNewSyncer) *Syncer {
	mock := &Syncer{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}
