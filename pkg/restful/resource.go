/*
Copyright 2017 The Nuclio Authors.

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

package restful

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/nuclio/nuclio/pkg/errors"
	"github.com/nuclio/nuclio/pkg/registry"

	"github.com/go-chi/chi"
	"github.com/nuclio/logger"
	"github.com/nuclio/nuclio-sdk-go"
)

type Attributes map[string]interface{}

// A custom route returns:
// resource type: string
// resources: a map of resource ID, resource attributes
// single: whether or not the resources should be treated as a single resource (if false, will be returned as list)
// status code: status code to return
// error: an error, if something went wrong
type CustomRouteFunc func(*http.Request) (string, map[string]Attributes, map[string]string, bool, int, error)

type CustomRoute struct {
	Pattern   string
	Method    string
	RouteFunc CustomRouteFunc
}

type Resource interface {

	// Initialize the concrete server
	Initialize(logger.Logger, Server) (chi.Router, error)

	// Called after initialization
	OnAfterInitialize() error

	// returns a list of custom routes for the resource
	GetCustomRoutes() ([]CustomRoute, error)

	// return all instances for resources with multiple instances
	GetAll(request *http.Request) (map[string]Attributes, error)

	// return specific instance by ID
	GetByID(request *http.Request, id string) (Attributes, error)

	// returns resource ID, attributes
	Create(request *http.Request) (string, Attributes, error)

	// returns attributes (optionally)
	Update(request *http.Request, id string) (Attributes, error)

	// delete an entity
	Delete(request *http.Request, id string) error
}

type ResourceMethod int

const (
	ResourceMethodGetList ResourceMethod = iota
	ResourceMethodGetDetail
	ResourceMethodCreate
	ResourceMethodUpdate
	ResourceMethodDelete
)

type AbstractResource struct {
	name            string
	Logger          logger.Logger
	router          chi.Router
	Resource        Resource
	resourceMethods []ResourceMethod
	server          Server
	encoderFactory  EncoderFactory
}

func NewAbstractResource(name string, resourceMethods []ResourceMethod) *AbstractResource {
	return &AbstractResource{
		name:            name,
		resourceMethods: resourceMethods,
		encoderFactory:  &JSONEncoderFactory{},
	}
}

func (ar *AbstractResource) Initialize(parentLogger logger.Logger, server Server) (chi.Router, error) {
	ar.Logger = parentLogger.GetChild(ar.name)

	ar.server = server
	ar.router = chi.NewRouter()

	// register routes based on supported methods
	ar.registerRoutes()

	ar.Resource.OnAfterInitialize()

	return ar.router, nil
}

func (ar *AbstractResource) Register(registry *registry.Registry) {
	registry.Register(ar.name, ar)
}

func (ar *AbstractResource) GetServer() Server {
	return ar.server
}

// called after initialization
func (ar *AbstractResource) OnAfterInitialize() error {
	return nil
}

// return all instances for resources with multiple instances
func (ar *AbstractResource) GetAll(request *http.Request) (map[string]Attributes, error) {
	return nil, nil
}

// return specific instance by ID
func (ar *AbstractResource) GetByID(request *http.Request, id string) (Attributes, error) {
	return nil, nil
}

// create a resource
func (ar *AbstractResource) Create(request *http.Request) (string, Attributes, error) {
	return "", nil, nuclio.ErrNotImplemented
}

func (ar *AbstractResource) Update(request *http.Request, id string) (Attributes, error) {
	return nil, nuclio.ErrNotImplemented
}

func (ar *AbstractResource) Delete(request *http.Request, id string) error {
	return nuclio.ErrNotImplemented
}

// returns a list of custom routes for the resource
func (ar *AbstractResource) GetCustomRoutes() ([]CustomRoute, error) {
	return []CustomRoute{}, nil
}

// for raw routes, those that don't return an attribute
func (ar *AbstractResource) GetRouter() chi.Router {
	return ar.router
}

func (ar *AbstractResource) registerRoutes() error {
	for _, resourceMethod := range ar.resourceMethods {
		switch resourceMethod {
		case ResourceMethodGetList:
			ar.router.Get("/", ar.handleGetList)
		case ResourceMethodGetDetail:
			ar.router.Get("/{id}", ar.handleGetDetails)
		case ResourceMethodCreate:
			ar.router.Post("/", ar.handleCreate)
		case ResourceMethodUpdate:
			ar.router.Put("/{id}", ar.handleUpdate)
		case ResourceMethodDelete:
			ar.router.Delete("/{id}", ar.handleDelete)
		}
	}

	return ar.registerCustomRoutes()
}

func (ar *AbstractResource) registerCustomRoutes() error {
	CustomRoutes, _ := ar.Resource.GetCustomRoutes()

	// not all resources support custom routes
	if CustomRoutes == nil {
		return nil
	}

	// iterate through the custom routes and register a handler for them
	for _, customRoute := range CustomRoutes {
		var routerFunc func(string, http.HandlerFunc)

		switch customRoute.Method {
		case http.MethodGet:
			routerFunc = ar.router.Get
		case http.MethodPost:
			routerFunc = ar.router.Post
		case http.MethodPut:
			routerFunc = ar.router.Put
		case http.MethodDelete:
			routerFunc = ar.router.Delete
		}

		customRouteCopy := customRoute

		ar.Logger.DebugWith("Registered custom route",
			"pattern", customRoute.Pattern,
			"method", customRoute.Method)

		routerFunc(customRoute.Pattern, func(responseWriter http.ResponseWriter, request *http.Request) {
			ar.callCustomRouteFunc(responseWriter, request, customRouteCopy.RouteFunc)
		})
	}

	return nil
}

func (ar *AbstractResource) handleGetList(responseWriter http.ResponseWriter, request *http.Request) {
	encoder := ar.encoderFactory.NewEncoder(responseWriter, ar.name)

	allResources, err := ar.Resource.GetAll(request)

	// if the error warranted writing a response or if there are no attributes - do nothing
	if ar.writeStatusCodeAndErrorReason(responseWriter, err, http.StatusOK) {
		return
	}

	if allResources == nil {
		allResources = map[string]Attributes{}
	}

	encoder.EncodeResources(allResources)
}

func (ar *AbstractResource) handleGetDetails(responseWriter http.ResponseWriter, request *http.Request) {

	// registered as "/:id/"
	resourceID := chi.URLParam(request, "id")

	// delegate to child
	attributes, err := ar.Resource.GetByID(request, resourceID)

	// if not found return 404
	if err == nil && attributes == nil {
		responseWriter.WriteHeader(http.StatusNotFound)
		return
	}

	// if the error warranted writing a response or if there are no attributes - do nothing
	if ar.writeStatusCodeAndErrorReason(responseWriter, err, http.StatusOK) {
		return
	}

	if attributes == nil {
		attributes = Attributes{}
	}

	ar.encoderFactory.NewEncoder(responseWriter, ar.name).EncodeResource(resourceID, attributes)
}

func (ar *AbstractResource) handleCreate(responseWriter http.ResponseWriter, request *http.Request) {

	// delegate to child
	resourceID, attributes, err := ar.Resource.Create(request)

	defaultStatusCode := http.StatusCreated
	if attributes == nil {
		defaultStatusCode = http.StatusNoContent
	}

	// if the error warranted writing a response or if there are no attributes - do nothing
	if ar.writeStatusCodeAndErrorReason(responseWriter, err, defaultStatusCode) || attributes == nil {
		return
	}

	ar.encoderFactory.NewEncoder(responseWriter, ar.name).EncodeResource(resourceID, attributes)
}

func (ar *AbstractResource) handleUpdate(responseWriter http.ResponseWriter, request *http.Request) {

	// registered as "/:id/"
	resourceID := chi.URLParam(request, "id")

	// delegate to child
	attributes, err := ar.Resource.Update(request, resourceID)

	defaultStatusCode := http.StatusOK
	if attributes == nil {
		defaultStatusCode = http.StatusNoContent
	}

	// if the error warranted writing a response or if there are no attributes - do nothing
	if ar.writeStatusCodeAndErrorReason(responseWriter, err, defaultStatusCode) || attributes == nil {
		return
	}

	ar.encoderFactory.NewEncoder(responseWriter, ar.name).EncodeResource(resourceID, attributes)
}

func (ar *AbstractResource) handleDelete(responseWriter http.ResponseWriter, request *http.Request) {

	// registered as "/:id/"
	resourceID := chi.URLParam(request, "id")

	// delegate to child
	err := ar.Resource.Delete(request, resourceID)

	// get the status code from the error
	ar.writeStatusCodeAndErrorReason(responseWriter, err, http.StatusNoContent)
}

func (ar *AbstractResource) callCustomRouteFunc(responseWriter http.ResponseWriter,
	request *http.Request,
	routeFunc CustomRouteFunc) {

	// see if the resource only supports a single record
	resourceType, resources, headers, single, statusCode, err := routeFunc(request)

	// set headers in response
	for headerKey, headerValue := range headers {
		responseWriter.Header().Set(headerKey, headerValue)
	}

	// if the error warranted writing a response or if there are no attributes - do nothing
	if ar.writeStatusCodeAndErrorReason(responseWriter, err, statusCode) {
		return
	}

	if resources == nil {

		// write a valid, empty JSON
		responseWriter.Write([]byte("{}"))

		return
	}

	encoder := ar.encoderFactory.NewEncoder(responseWriter, resourceType)

	if single {

		// to get the first, we must iterate over range
		for resourceKey, resourceAttributes := range resources {
			if resourceAttributes != nil {
				encoder.EncodeResource(resourceKey, resourceAttributes)
			}

			break
		}

	} else {
		encoder.EncodeResources(resources)
	}
}

// returns "false" if did not write the actual response, true if it did
func (ar *AbstractResource) writeErrorReason(responseWriter io.Writer, err error) {
	if err == nil {
		return
	}

	// to hold the error
	buffer := bytes.Buffer{}

	// there can be three types of errors here:
	// 1. a basic golang error, if the user returned something like errors.New("Whatever")
	// 2. a pkg/error, if the user returned errors.Wrap(...)
	// 3. a nuclio.ErrorWithStatusCode

	// if the error is with status code, get the underlying error. otherwise, PrintErrorStack fails the type
	// assertion that ErrorWithStatusCode is of type errors.Error
	switch typedErr := err.(type) {
	case nuclio.ErrorWithStatusCode:
		err = typedErr.GetError()
	case *nuclio.ErrorWithStatusCode:
		err = typedErr.GetError()
	}

	// try to get the error stack
	errors.PrintErrorStack(&buffer, err, 10)

	// format to json manually
	serializedError, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{
		buffer.String(),
	})

	// write to the response
	responseWriter.Write(serializedError)
}

func (ar *AbstractResource) getStatusCodeFromError(err error, defaultStatusCode int) int {
	if err == nil {
		return defaultStatusCode
	}

	// see if the user returned an error with status code
	switch typedError := err.(type) {
	case nuclio.ErrorWithStatusCode:
		return typedError.StatusCode()
	case *nuclio.ErrorWithStatusCode:
		return typedError.StatusCode()
	case *errors.Error:
		return http.StatusInternalServerError
	default:
		return defaultStatusCode
	}
}

func (ar *AbstractResource) statusCodeIsError(statusCode int) bool {
	return statusCode >= 400
}

// write error and status code if applicable
func (ar *AbstractResource) writeStatusCodeAndErrorReason(responseWriter http.ResponseWriter,
	err error,
	defaultStatusCode int) bool {

	// get the status code from the error
	statusCode := ar.getStatusCodeFromError(err, defaultStatusCode)
	responseWriter.WriteHeader(statusCode)

	// if the status code is an actual error, write the error reason and return
	if ar.statusCodeIsError(statusCode) {
		ar.writeErrorReason(responseWriter, err)

		return true
	}

	return false
}
