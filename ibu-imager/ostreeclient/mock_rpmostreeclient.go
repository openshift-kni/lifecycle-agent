// Code generated by MockGen. DO NOT EDIT.
// Source: rpmostreeclient.go

// Package rpmostreeclient is a generated GoMock package.
package rpmostreeclient

import (
	gomock "github.com/golang/mock/gomock"
	reflect "reflect"
)

// MockIClient is a mock of IClient interface
type MockIClient struct {
	ctrl     *gomock.Controller
	recorder *MockIClientMockRecorder
}

// MockIClientMockRecorder is the mock recorder for MockIClient
type MockIClientMockRecorder struct {
	mock *MockIClient
}

// NewMockIClient creates a new mock instance
func NewMockIClient(ctrl *gomock.Controller) *MockIClient {
	mock := &MockIClient{ctrl: ctrl}
	mock.recorder = &MockIClientMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use
func (m *MockIClient) EXPECT() *MockIClientMockRecorder {
	return m.recorder
}

// newCmd mocks base method
func (m *MockIClient) newCmd(args ...string) []byte {
	m.ctrl.T.Helper()
	varargs := []interface{}{}
	for _, a := range args {
		varargs = append(varargs, a)
	}
	ret := m.ctrl.Call(m, "newCmd", varargs...)
	ret0, _ := ret[0].([]byte)
	return ret0
}

// newCmd indicates an expected call of newCmd
func (mr *MockIClientMockRecorder) newCmd(args ...interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "newCmd", reflect.TypeOf((*MockIClient)(nil).newCmd), args...)
}

// RpmOstreeVersion mocks base method
func (m *MockIClient) RpmOstreeVersion() (*VersionData, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "RpmOstreeVersion")
	ret0, _ := ret[0].(*VersionData)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// RpmOstreeVersion indicates an expected call of RpmOstreeVersion
func (mr *MockIClientMockRecorder) RpmOstreeVersion() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "RpmOstreeVersion", reflect.TypeOf((*MockIClient)(nil).RpmOstreeVersion))
}

// QueryStatus mocks base method
func (m *MockIClient) QueryStatus() (*Status, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "QueryStatus")
	ret0, _ := ret[0].(*Status)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// QueryStatus indicates an expected call of QueryStatus
func (mr *MockIClientMockRecorder) QueryStatus() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "QueryStatus", reflect.TypeOf((*MockIClient)(nil).QueryStatus))
}

// IsStaterootBooted mocks base method
func (m *MockIClient) IsStaterootBooted(stateroot string) (bool, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "IsStaterootBooted", stateroot)
	ret0, _ := ret[0].(bool)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// IsStaterootBooted indicates an expected call of IsStaterootBooted
func (mr *MockIClientMockRecorder) IsStaterootBooted(stateroot interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "IsStaterootBooted", reflect.TypeOf((*MockIClient)(nil).IsStaterootBooted), stateroot)
}
