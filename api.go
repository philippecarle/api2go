package api2go

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/golang/gddo/httputil"
	"github.com/julienschmidt/httprouter"
	"github.com/manyminds/api2go/jsonapi"
)

// The CRUD interface MUST be implemented in order to use the api2go api.
type CRUD interface {
	// FindOne returns an object by its ID
	FindOne(ID string, req Request) (interface{}, error)

	// Create a new object and return its ID
	Create(obj interface{}, req Request) (string, error)

	// Delete an object
	Delete(id string, req Request) error

	// Update an object
	Update(obj interface{}, req Request) error
}

// The FindAll interface can be optionally implemented to fetch all records at once.
type FindAll interface {
	// FindAll returns all objects
	FindAll(req Request) (interface{}, error)
}

// The PaginatedFindAll interface can be optionally implemented to fetch a subset of all records.
// Pagination query parameters must be used to limit the result. Pagination URLs will automatically
// be generated by the api. You can use a combination of the following 2 query parameters:
// page[number] AND page[size]
// OR page[offset] AND page[limit]
type PaginatedFindAll interface {
	PaginatedFindAll(req Request) (obj interface{}, totalCount uint, err error)
}

type paginationQueryParams struct {
	number, size, offset, limit string
}

func newPaginationQueryParams(r *http.Request) paginationQueryParams {
	var result paginationQueryParams

	queryParams := r.URL.Query()
	result.number = queryParams.Get("page[number]")
	result.size = queryParams.Get("page[size]")
	result.offset = queryParams.Get("page[offset]")
	result.limit = queryParams.Get("page[limit]")

	return result
}

func (p paginationQueryParams) isValid() bool {
	if p.number == "" && p.size == "" && p.offset == "" && p.limit == "" {
		return false
	}

	if p.number != "" && p.size != "" && p.offset == "" && p.limit == "" {
		return true
	}

	if p.number == "" && p.size == "" && p.offset != "" && p.limit != "" {
		return true
	}

	return false
}

func (p paginationQueryParams) getLinks(r *http.Request, count uint, info information) (result map[string]string, err error) {
	result = make(map[string]string)

	params := r.URL.Query()
	prefix := ""
	baseURL := info.GetBaseURL()
	if baseURL != "" {
		prefix = baseURL
	}
	requestURL := fmt.Sprintf("%s%s", prefix, r.URL.Path)

	if p.number != "" {
		// we have number & size params
		var number uint64
		number, err = strconv.ParseUint(p.number, 10, 64)
		if err != nil {
			return
		}

		if p.number != "1" {
			params.Set("page[number]", "1")
			query, _ := url.QueryUnescape(params.Encode())
			result["first"] = fmt.Sprintf("%s?%s", requestURL, query)

			params.Set("page[number]", strconv.FormatUint(number-1, 10))
			query, _ = url.QueryUnescape(params.Encode())
			result["prev"] = fmt.Sprintf("%s?%s", requestURL, query)
		}

		// calculate last page number
		var size uint64
		size, err = strconv.ParseUint(p.size, 10, 64)
		if err != nil {
			return
		}
		totalPages := (uint64(count) / size)
		if (uint64(count) % size) != 0 {
			// there is one more page with some len(items) < size
			totalPages++
		}

		if number != totalPages {
			params.Set("page[number]", strconv.FormatUint(number+1, 10))
			query, _ := url.QueryUnescape(params.Encode())
			result["next"] = fmt.Sprintf("%s?%s", requestURL, query)

			params.Set("page[number]", strconv.FormatUint(totalPages, 10))
			query, _ = url.QueryUnescape(params.Encode())
			result["last"] = fmt.Sprintf("%s?%s", requestURL, query)
		}
	} else {
		// we have offset & limit params
		var offset, limit uint64
		offset, err = strconv.ParseUint(p.offset, 10, 64)
		if err != nil {
			return
		}
		limit, err = strconv.ParseUint(p.limit, 10, 64)
		if err != nil {
			return
		}

		if p.offset != "0" {
			params.Set("page[offset]", "0")
			query, _ := url.QueryUnescape(params.Encode())
			result["first"] = fmt.Sprintf("%s?%s", requestURL, query)

			var prevOffset uint64
			if limit > offset {
				prevOffset = 0
			} else {
				prevOffset = offset - limit
			}
			params.Set("page[offset]", strconv.FormatUint(prevOffset, 10))
			query, _ = url.QueryUnescape(params.Encode())
			result["prev"] = fmt.Sprintf("%s?%s", requestURL, query)
		}

		// check if there are more entries to be loaded
		if (offset + limit) < uint64(count) {
			params.Set("page[offset]", strconv.FormatUint(offset+limit, 10))
			query, _ := url.QueryUnescape(params.Encode())
			result["next"] = fmt.Sprintf("%s?%s", requestURL, query)

			params.Set("page[offset]", strconv.FormatUint(uint64(count)-limit, 10))
			query, _ = url.QueryUnescape(params.Encode())
			result["last"] = fmt.Sprintf("%s?%s", requestURL, query)
		}
	}

	return
}

// API is a REST JSONAPI.
type API struct {
	router *httprouter.Router
	// Route prefix, including slashes
	prefix     string
	info       information
	resources  []resource
	marshalers map[string]ContentMarshaler
}

type information struct {
	prefix  string
	baseURL string
}

func (i information) GetBaseURL() string {
	return i.baseURL
}

func (i information) GetPrefix() string {
	return i.prefix
}

// NewAPI returns an initialized API instance
// `prefix` is added in front of all endpoints.
func NewAPI(prefix string) *API {
	// Add initial and trailing slash to prefix
	prefixSlashes := strings.Trim(prefix, "/")
	if len(prefixSlashes) > 0 {
		prefixSlashes = "/" + prefixSlashes + "/"
	} else {
		prefixSlashes = "/"
	}

	return &API{
		router:     httprouter.New(),
		prefix:     prefixSlashes,
		info:       information{prefix: prefix},
		marshalers: DefaultContentMarshalers,
	}
}

// NewAPIWithBaseURL does the same as NewAPI with the addition of
// a baseURL which get's added in front of all generated URLs.
// For example http://localhost/v1/myResource/abc instead of /v1/myResource/abc
func NewAPIWithBaseURL(prefix string, baseURL string) *API {
	api := NewAPI(prefix)
	api.info.baseURL = baseURL

	return api
}

// ContentMarshaler controls how requests from clients are unmarshaled
// and responses from the server are marshaled. The content marshaler
// is in charge of encoding and decoding data to and from a particular
// format (e.g. JSON). The encoding and decoding processes follow the
// rules of the standard encoding/json package.
type ContentMarshaler interface {
	Marshal(i interface{}) ([]byte, error)
	Unmarshal(data []byte, i interface{}) error
}

// JSONContentMarshaler uses the standard encoding/json package for
// decoding requests and encoding responses in JSON format.
type JSONContentMarshaler struct {
}

// Marshal marshals with default JSON
func (m JSONContentMarshaler) Marshal(i interface{}) ([]byte, error) {
	return json.Marshal(i)
}

// Unmarshal with default JSON
func (m JSONContentMarshaler) Unmarshal(data []byte, i interface{}) error {
	return json.Unmarshal(data, i)
}

// DefaultContentMarshalers is the default set of content marshalers for an API.
// Currently this means handling application/vnd.api+json content type bodies
// using the standard encoding/json package.
var DefaultContentMarshalers = map[string]ContentMarshaler{
	"application/vnd.api+json": JSONContentMarshaler{},
}

// NewAPIWithMarshalers does the same as NewAPIWithBaseURL with the addition
// of a set of marshalers that provide a way to interact with clients that
// use a serialization format other than JSON. The marshalers map is indexed
// by the MIME content type to use for a given request-response pair. If the
// client provides an Accept header the server will respond using the client's
// preferred content type, otherwise it will respond using whatever content
// type the client provided in its Content-Type request header.
func NewAPIWithMarshalers(prefix string, baseURL string, marshalers map[string]ContentMarshaler) *API {
	if len(marshalers) == 0 {
		panic("marshaler map must not be empty")
	}

	api := NewAPIWithBaseURL(prefix, baseURL)
	api.marshalers = marshalers

	return api
}

//SetRedirectTrailingSlash enables 307 redirects on urls ending with /
//when disabled, an URL ending with / will 404
func (api *API) SetRedirectTrailingSlash(enabled bool) {
	if api.router == nil {
		panic("router must not be nil")
	}

	api.router.RedirectTrailingSlash = enabled
}

// Request holds additional information for FindOne and Find Requests
type Request struct {
	PlainRequest *http.Request
	QueryParams  map[string][]string
	Header       http.Header
}

type resource struct {
	resourceType reflect.Type
	source       CRUD
	name         string
	marshalers   map[string]ContentMarshaler
}

func (api *API) addResource(prototype jsonapi.MarshalIdentifier, source CRUD, marshalers map[string]ContentMarshaler) *resource {
	resourceType := reflect.TypeOf(prototype)
	if resourceType.Kind() != reflect.Struct && resourceType.Kind() != reflect.Ptr {
		panic("pass an empty resource struct or a struct pointer to AddResource!")
	}

	var ptrPrototype interface{}
	var name string

	if resourceType.Kind() == reflect.Struct {
		ptrPrototype = reflect.New(resourceType).Interface()
		name = resourceType.Name()
	} else {
		ptrPrototype = reflect.ValueOf(prototype).Interface()
		name = resourceType.Elem().Name()
	}

	name = jsonapi.Jsonify(jsonapi.Pluralize(name))

	res := resource{
		resourceType: resourceType,
		name:         name,
		source:       source,
		marshalers:   marshalers,
	}

	api.router.Handle("OPTIONS", api.prefix+name, func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		w.Header().Set("Allow", "GET,POST,PATCH,OPTIONS")
		w.WriteHeader(http.StatusNoContent)
	})

	api.router.Handle("OPTIONS", api.prefix+name+"/:id", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		w.Header().Set("Allow", "GET,PATCH,DELETE,OPTIONS")
		w.WriteHeader(http.StatusNoContent)
	})

	api.router.GET(api.prefix+name, func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		err := res.handleIndex(w, r, api.info)
		if err != nil {
			handleError(err, w, r, marshalers)
		}
	})

	api.router.GET(api.prefix+name+"/:id", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		err := res.handleRead(w, r, ps, api.info)
		if err != nil {
			handleError(err, w, r, marshalers)
		}
	})

	// generate all routes for linked relations if there are relations
	casted, ok := prototype.(jsonapi.MarshalReferences)
	if ok {
		relations := casted.GetReferences()
		for _, relation := range relations {
			api.router.GET(api.prefix+name+"/:id/relationships/"+relation.Name, func(relation jsonapi.Reference) httprouter.Handle {
				return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
					err := res.handleReadRelation(w, r, ps, api.info, relation)
					if err != nil {
						handleError(err, w, r, marshalers)
					}
				}
			}(relation))

			api.router.GET(api.prefix+name+"/:id/"+relation.Name, func(relation jsonapi.Reference) httprouter.Handle {
				return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
					err := res.handleLinked(api, w, r, ps, relation, api.info)
					if err != nil {
						handleError(err, w, r, marshalers)
					}
				}
			}(relation))

			api.router.PATCH(api.prefix+name+"/:id/relationships/"+relation.Name, func(relation jsonapi.Reference) httprouter.Handle {
				return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
					err := res.handleReplaceRelation(w, r, ps, relation)
					if err != nil {
						handleError(err, w, r, marshalers)
					}
				}
			}(relation))

			if _, ok := ptrPrototype.(jsonapi.EditToManyRelations); ok && relation.Name == jsonapi.Pluralize(relation.Name) {
				// generate additional routes to manipulate to-many relationships
				api.router.POST(api.prefix+name+"/:id/relationships/"+relation.Name, func(relation jsonapi.Reference) httprouter.Handle {
					return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
						err := res.handleAddToManyRelation(w, r, ps, relation)
						if err != nil {
							handleError(err, w, r, marshalers)
						}
					}
				}(relation))

				api.router.DELETE(api.prefix+name+"/:id/relationships/"+relation.Name, func(relation jsonapi.Reference) httprouter.Handle {
					return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
						err := res.handleDeleteToManyRelation(w, r, ps, relation)
						if err != nil {
							handleError(err, w, r, marshalers)
						}
					}
				}(relation))
			}
		}
	}

	api.router.POST(api.prefix+name, func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		err := res.handleCreate(w, r, api.prefix, api.info)
		if err != nil {
			handleError(err, w, r, marshalers)
		}
	})

	api.router.DELETE(api.prefix+name+"/:id", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		err := res.handleDelete(w, r, ps)
		if err != nil {
			handleError(err, w, r, marshalers)
		}
	})

	api.router.PATCH(api.prefix+name+"/:id", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		err := res.handleUpdate(w, r, ps)
		if err != nil {
			handleError(err, w, r, marshalers)
		}
	})

	api.resources = append(api.resources, res)

	return &res
}

// AddResource registers a data source for the given resource
// At least the CRUD interface must be implemented, all the other interfaces are optional.
// `resource` should be either an empty struct instance such as `Post{}` or a pointer to
// a struct such as `&Post{}`. The same type will be used for constructing new elements.
func (api *API) AddResource(prototype jsonapi.MarshalIdentifier, source CRUD) {
	api.addResource(prototype, source, api.marshalers)
}

func buildRequest(r *http.Request) Request {
	req := Request{PlainRequest: r}
	params := make(map[string][]string)
	for key, values := range r.URL.Query() {
		params[key] = strings.Split(values[0], ",")
	}
	req.QueryParams = params
	req.Header = r.Header
	return req
}

func (res *resource) handleIndex(w http.ResponseWriter, r *http.Request, info information) error {
	pagination := newPaginationQueryParams(r)
	if pagination.isValid() {
		source, ok := res.source.(PaginatedFindAll)
		if !ok {
			return NewHTTPError(nil, "Resource does not implement the PaginatedFindAll interface", http.StatusNotFound)
		}

		var count uint
		objs, count, err := source.PaginatedFindAll(buildRequest(r))
		if err != nil {
			return err
		}

		paginationLinks, err := pagination.getLinks(r, count, info)
		if err != nil {
			return err
		}

		return respondWithPagination(objs, info, http.StatusOK, paginationLinks, w, r, res.marshalers)
	}
	source, ok := res.source.(FindAll)
	if !ok {
		return NewHTTPError(nil, "Resource does not implement the FindAll interface", http.StatusNotFound)
	}

	objs, err := source.FindAll(buildRequest(r))
	if err != nil {
		return err
	}

	return respondWith(objs, info, http.StatusOK, w, r, res.marshalers)
}

func (res *resource) handleRead(w http.ResponseWriter, r *http.Request, ps httprouter.Params, info information) error {
	id := ps.ByName("id")

	obj, err := res.source.FindOne(id, buildRequest(r))

	if err != nil {
		return err
	}

	return respondWith(obj, info, http.StatusOK, w, r, res.marshalers)
}

func (res *resource) handleReadRelation(w http.ResponseWriter, r *http.Request, ps httprouter.Params, info information, relation jsonapi.Reference) error {
	id := ps.ByName("id")

	obj, err := res.source.FindOne(id, buildRequest(r))
	if err != nil {
		return err
	}

	internalError := NewHTTPError(nil, "Internal server error, invalid object structure", http.StatusInternalServerError)

	marshalled, err := jsonapi.MarshalWithURLs(obj, info)
	data, ok := marshalled["data"]
	if !ok {
		return internalError
	}
	relationships, ok := data.(map[string]interface{})["relationships"]
	if !ok {
		return internalError
	}

	rel, ok := relationships.(map[string]map[string]interface{})[relation.Name]
	if !ok {
		return NewHTTPError(nil, fmt.Sprintf("There is no relation with the name %s", relation.Name), http.StatusNotFound)
	}
	links, ok := rel["links"].(map[string]string)
	if !ok {
		return internalError
	}
	self, ok := links["self"]
	if !ok {
		return internalError
	}
	related, ok := links["related"]
	if !ok {
		return internalError
	}
	relationData, ok := rel["data"]
	if !ok {
		return internalError
	}

	result := map[string]interface{}{}
	result["links"] = map[string]interface{}{
		"self":    self,
		"related": related,
	}
	result["data"] = relationData

	return marshalResponse(result, w, http.StatusOK, r, res.marshalers)
}

// try to find the referenced resource and call the findAll Method with referencing resource id as param
func (res *resource) handleLinked(api *API, w http.ResponseWriter, r *http.Request, ps httprouter.Params, linked jsonapi.Reference, info information) error {
	id := ps.ByName("id")
	for _, resource := range api.resources {
		if resource.name == linked.Type {
			request := buildRequest(r)
			request.QueryParams[res.name+"ID"] = []string{id}

			// check for pagination, otherwise normal FindAll
			pagination := newPaginationQueryParams(r)
			if pagination.isValid() {
				source, ok := resource.source.(PaginatedFindAll)
				if !ok {
					return NewHTTPError(nil, "Resource does not implement the PaginatedFindAll interface", http.StatusNotFound)
				}

				var count uint
				objs, count, err := source.PaginatedFindAll(request)
				if err != nil {
					return err
				}

				paginationLinks, err := pagination.getLinks(r, count, info)
				if err != nil {
					return err
				}

				return respondWithPagination(objs, info, http.StatusOK, paginationLinks, w, r, res.marshalers)
			}

			source, ok := resource.source.(FindAll)
			if !ok {
				return NewHTTPError(nil, "Resource does not implement the FindAll interface", http.StatusNotFound)
			}

			obj, err := source.FindAll(request)
			if err != nil {
				return err
			}
			return respondWith(obj, info, http.StatusOK, w, r, res.marshalers)
		}
	}

	err := Error{
		Status: string(http.StatusNotFound),
		Title:  "Not Found",
		Detail: "No resource handler is registered to handle the linked resource " + linked.Name,
	}
	return respondWith(err, info, http.StatusNotFound, w, r, res.marshalers)
}

func (res *resource) handleCreate(w http.ResponseWriter, r *http.Request, prefix string, info information) error {
	ctx, err := unmarshalRequest(r, res.marshalers)
	if err != nil {
		return err
	}
	newObjs := reflect.MakeSlice(reflect.SliceOf(res.resourceType), 0, 0)

	structType := res.resourceType
	if structType.Kind() == reflect.Ptr {
		structType = structType.Elem()
	}

	err = jsonapi.UnmarshalInto(ctx, structType, &newObjs)
	if err != nil {
		return err
	}
	if newObjs.Len() != 1 {
		return errors.New("expected one object in POST")
	}

	//TODO create multiple objects not only one.
	newObj := newObjs.Index(0).Interface()

	id, err := res.source.Create(newObj, buildRequest(r))
	if err != nil {
		return err
	}
	w.Header().Set("Location", prefix+res.name+"/"+id)

	obj, err := res.source.FindOne(id, buildRequest(r))
	if err != nil {
		return err
	}

	return respondWith(obj, info, http.StatusCreated, w, r, res.marshalers)
}

func (res *resource) handleUpdate(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
	obj, err := res.source.FindOne(ps.ByName("id"), buildRequest(r))
	if err != nil {
		return err
	}

	ctx, err := unmarshalRequest(r, res.marshalers)
	if err != nil {
		return err
	}

	data, ok := ctx["data"]

	if !ok {
		return NewHTTPError(
			errors.New("Forbidden"),
			"missing mandatory data key.",
			http.StatusForbidden,
		)
	}

	check, ok := data.(map[string]interface{})
	if !ok {
		return NewHTTPError(
			errors.New("Forbidden"),
			"data must contain an object.",
			http.StatusForbidden,
		)
	}

	if _, ok := check["id"]; !ok {
		return NewHTTPError(
			errors.New("Forbidden"),
			"missing mandatory id key.",
			http.StatusForbidden,
		)
	}

	if _, ok := check["type"]; !ok {
		return NewHTTPError(
			errors.New("Forbidden"),
			"missing mandatory type key.",
			http.StatusForbidden,
		)
	}

	updatingObjs := reflect.MakeSlice(reflect.SliceOf(res.resourceType), 1, 1)
	updatingObjs.Index(0).Set(reflect.ValueOf(obj))

	structType := res.resourceType
	if structType.Kind() == reflect.Ptr {
		structType = structType.Elem()
	}

	err = jsonapi.UnmarshalInto(ctx, structType, &updatingObjs)
	if err != nil {
		return err
	}
	if updatingObjs.Len() != 1 {
		return errors.New("expected one object")
	}

	updatingObj := updatingObjs.Index(0).Interface()

	if err := res.source.Update(updatingObj, buildRequest(r)); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (res *resource) handleReplaceRelation(w http.ResponseWriter, r *http.Request, ps httprouter.Params, relation jsonapi.Reference) error {
	oldObj, err := res.source.FindOne(ps.ByName("id"), buildRequest(r))
	if err != nil {
		return err
	}

	inc, err := unmarshalRequest(r, res.marshalers)
	if err != nil {
		return err
	}

	data, ok := inc["data"]
	if !ok {
		return errors.New("Invalid object. Need a \"data\" object")
	}

	editObj, updateObj := getRelationUpdateObjects(oldObj)

	err = jsonapi.UnmarshalRelationshipsData(editObj, relation.Name, data)
	if err != nil {
		return err
	}

	if err := res.source.Update(updateObj, buildRequest(r)); err != nil {
		return err
	}

	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (res *resource) handleAddToManyRelation(w http.ResponseWriter, r *http.Request, ps httprouter.Params, relation jsonapi.Reference) error {
	oldObj, err := res.source.FindOne(ps.ByName("id"), buildRequest(r))
	if err != nil {
		return err
	}

	inc, err := unmarshalRequest(r, res.marshalers)
	if err != nil {
		return err
	}

	data, ok := inc["data"]
	if !ok {
		return errors.New("Invalid object. Need a \"data\" object")
	}

	editObj, updateObj := getRelationUpdateObjects(oldObj)

	newRels, ok := data.([]interface{})
	if !ok {
		return fmt.Errorf("Data must be an array with \"id\" and \"type\" field to add new to-many relationships")
	}

	newIDs := []string{}

	for _, newRel := range newRels {
		casted, ok := newRel.(map[string]interface{})
		if !ok {
			return errors.New("entry in data object invalid")
		}
		newID, ok := casted["id"].(string)
		if !ok {
			return errors.New("no id field found inside data object")
		}

		newIDs = append(newIDs, newID)
	}

	targetObj, ok := editObj.(jsonapi.EditToManyRelations)
	if !ok {
		return errors.New("target struct must implement jsonapi.EditToManyRelations")
	}
	targetObj.AddToManyIDs(relation.Name, newIDs)

	if err := res.source.Update(updateObj, buildRequest(r)); err != nil {
		return err
	}

	w.WriteHeader(http.StatusNoContent)

	return nil
}

func (res *resource) handleDeleteToManyRelation(w http.ResponseWriter, r *http.Request, ps httprouter.Params, relation jsonapi.Reference) error {
	oldObj, err := res.source.FindOne(ps.ByName("id"), buildRequest(r))
	if err != nil {
		return err
	}

	inc, err := unmarshalRequest(r, res.marshalers)
	if err != nil {
		return err
	}

	data, ok := inc["data"]
	if !ok {
		return errors.New("Invalid object. Need a \"data\" object")
	}

	editObj, updateObj := getRelationUpdateObjects(oldObj)

	newRels, ok := data.([]interface{})
	if !ok {
		return fmt.Errorf("Data must be an array with \"id\" and \"type\" field to add new to-many relationships")
	}

	obsoleteIDs := []string{}

	for _, newRel := range newRels {
		casted, ok := newRel.(map[string]interface{})
		if !ok {
			return errors.New("entry in data object invalid")
		}
		obsoleteID, ok := casted["id"].(string)
		if !ok {
			return errors.New("no id field found inside data object")
		}

		obsoleteIDs = append(obsoleteIDs, obsoleteID)
	}

	targetObj, ok := editObj.(jsonapi.EditToManyRelations)
	if !ok {
		return errors.New("target struct must implement jsonapi.EditToManyRelations")
	}
	targetObj.DeleteToManyIDs(relation.Name, obsoleteIDs)

	if err := res.source.Update(updateObj, buildRequest(r)); err != nil {
		return err
	}

	w.WriteHeader(http.StatusNoContent)

	return nil
}

// makes a copy of oldObj and returns two references to it, one that can be used to edit relations on,
// and one that can be used to call CRUD.Update with.
func getRelationUpdateObjects(oldObj interface{}) (editObj interface{}, updateObj interface{}) {
	if resType := reflect.TypeOf(oldObj); resType.Kind() == reflect.Struct {
		ptr := reflect.New(resType)
		ptr.Elem().Set(reflect.ValueOf(oldObj))

		// for struct resources, we pass a pointer to edit and a struct to Update
		editObj = ptr.Interface()
		updateObj = ptr.Elem().Interface()
	} else {
		ptr := reflect.New(resType.Elem())
		ptr.Elem().Set(reflect.ValueOf(oldObj).Elem())

		// for pointer resources, we pass a pointer both to edit and Update
		editObj = ptr.Interface()
		updateObj = ptr.Interface()
	}

	return
}

func (res *resource) handleDelete(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
	err := res.source.Delete(ps.ByName("id"), buildRequest(r))
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func writeResult(w http.ResponseWriter, data []byte, status int, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	w.Write(data)
}

func respondWith(obj interface{}, info information, status int, w http.ResponseWriter, r *http.Request, marshalers map[string]ContentMarshaler) error {
	data, err := jsonapi.MarshalWithURLs(obj, info)
	if err != nil {
		return err
	}

	return marshalResponse(data, w, status, r, marshalers)
}

func respondWithPagination(obj interface{}, info information, status int, links map[string]string, w http.ResponseWriter, r *http.Request, marshalers map[string]ContentMarshaler) error {
	data, err := jsonapi.MarshalWithURLs(obj, info)
	if err != nil {
		return err
	}

	data["links"] = links
	return marshalResponse(data, w, status, r, marshalers)
}

func unmarshalRequest(r *http.Request, marshalers map[string]ContentMarshaler) (map[string]interface{}, error) {
	defer r.Body.Close()
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	result := map[string]interface{}{}
	marshaler, _ := selectContentMarshaler(r, marshalers)
	err = marshaler.Unmarshal(data, &result)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func marshalResponse(resp interface{}, w http.ResponseWriter, status int, r *http.Request, marshalers map[string]ContentMarshaler) error {
	marshaler, contentType := selectContentMarshaler(r, marshalers)
	result, err := marshaler.Marshal(resp)
	if err != nil {
		return err
	}
	writeResult(w, result, status, contentType)
	return nil
}

func selectContentMarshaler(r *http.Request, marshalers map[string]ContentMarshaler) (marshaler ContentMarshaler, contentType string) {
	if _, found := r.Header["Accept"]; found {
		var contentTypes []string
		for ct := range marshalers {
			contentTypes = append(contentTypes, ct)
		}

		contentType = httputil.NegotiateContentType(r, contentTypes, "application/vnd.api+json")
		marshaler = marshalers[contentType]
	} else if contentTypes, found := r.Header["Content-Type"]; found {
		contentType = contentTypes[0]
		marshaler = marshalers[contentType]
	}

	if marshaler == nil {
		contentType = "application/vnd.api+json"
		marshaler = JSONContentMarshaler{}
	}

	return
}

func handleError(err error, w http.ResponseWriter, r *http.Request, marshalers map[string]ContentMarshaler) {
	marshaler, contentType := selectContentMarshaler(r, marshalers)

	log.Println(err)
	if e, ok := err.(HTTPError); ok {
		writeResult(w, []byte(marshalError(e, marshaler)), e.status, contentType)
		return

	}

	writeResult(w, []byte(marshalError(err, marshaler)), http.StatusInternalServerError, contentType)
}

// Handler returns the http.Handler instance for the API.
func (api *API) Handler() http.Handler {
	return api.router
}

// Router can be used instead of Handler() to get the instance of julienschmidt httprouter.
func (api *API) Router() *httprouter.Router {
	return api.router
}
