# Distribution Registry - Comprehensive Technical Analysis

This document provides a detailed technical analysis of how the Distribution registry code works, including function calls, architecture patterns, and data flows.

## Table of Contents

1. [Architecture Overview](#architecture-overview)
2. [Application Startup and Initialization](#application-startup-and-initialization)
3. [HTTP Request Processing Pipeline](#http-request-processing-pipeline)
4. [Storage Layer Architecture](#storage-layer-architecture)
5. [Authentication and Authorization System](#authentication-and-authorization-system)
6. [Manifest and Blob Operations](#manifest-and-blob-operations)
7. [Key Design Patterns](#key-design-patterns)
8. [Function Call Flows](#function-call-flows)

## Architecture Overview

The Distribution registry is built using a layered architecture with clear separation of concerns:

```
┌─────────────────────────────────────────┐
│           HTTP API Layer                │ ← Handlers, Routes, Middleware
├─────────────────────────────────────────┤
│         Business Logic Layer            │ ← Repository, Manifest, Blob services
├─────────────────────────────────────────┤
│          Storage Layer                  │ ← Abstract storage interface
├─────────────────────────────────────────┤
│         Storage Drivers                 │ ← Filesystem, S3, Azure, GCS
└─────────────────────────────────────────┘
```

## Application Startup and Initialization

### Entry Point: `cmd/registry/main.go`

```go
func main() {
    // Import side effects register storage drivers and auth providers
    _ "github.com/distribution/distribution/v3/registry/auth/htpasswd"
    _ "github.com/distribution/distribution/v3/registry/auth/silly"
    _ "github.com/distribution/distribution/v3/registry/auth/token"
    _ "github.com/distribution/distribution/v3/registry/storage/driver/filesystem"
    _ "github.com/distribution/distribution/v3/registry/storage/driver/s3-aws"
    // ... other drivers
    
    registry.RootCmd.Execute()  // Cobra CLI execution
}
```

### Command Setup: `registry/root.go`

```go
var RootCmd = &cobra.Command{
    Use:   "registry",
    Short: "`registry`",
    Long:  "`registry`",
    Run: func(cmd *cobra.Command, args []string) {
        if showVersion {
            version.PrintVersion()
            return
        }
        cmd.Usage()
    },
}

func init() {
    RootCmd.AddCommand(ServeCmd)    // Main server command
    RootCmd.AddCommand(GCCmd)       // Garbage collection command
}
```

### Server Command: `registry/registry.go:ServeCmd`

```go
var ServeCmd = &cobra.Command{
    Use:   "serve <config>",
    Short: "`serve` stores and distributes Docker images",
    Run: func(cmd *cobra.Command, args []string) {
        // 1. Setup context with version
        ctx := dcontext.WithVersion(dcontext.Background(), version.Version())
        
        // 2. Parse configuration
        config, err := resolveConfiguration(args)
        if err != nil {
            fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
            os.Exit(1)
        }
        
        // 3. Create registry instance
        registry, err := NewRegistry(ctx, config)
        if err != nil {
            logrus.Fatalln(err)
        }
        
        // 4. Configure debug server (optional)
        configureDebugServer(config)
        
        // 5. Start serving
        if err = registry.ListenAndServe(); err != nil {
            logrus.Fatalln(err)
        }
    },
}
```

### Registry Creation: `NewRegistry()` Function

```go
func NewRegistry(ctx context.Context, config *configuration.Configuration) (*Registry, error) {
    // 1. Configure logging
    ctx, err = configureLogging(ctx, config)
    
    // 2. Create application instance
    app := handlers.NewApp(ctx, config)
    
    // 3. Register health checks
    app.RegisterHealthChecks()
    
    // 4. Build HTTP handler chain
    var handler http.Handler = app
    handler = alive("/", handler)                    // Basic liveness endpoint
    handler = health.Handler(handler)               // Health check middleware
    handler = panicHandler(handler)                 // Panic recovery
    if !config.Log.AccessLog.Disabled {
        handler = gorhandlers.CombinedLoggingHandler(os.Stdout, handler)
    }
    
    // 5. Apply custom middleware
    for _, applyHandlerMiddleware := range handlerMiddlewares {
        handler = applyHandlerMiddleware(config, handler)
    }
    
    // 6. Initialize OpenTelemetry
    err = tracing.InitOpenTelemetry(app.Context)
    
    // 7. Setup HTTP/2 support
    if config.HTTP.H2C.Enabled {
        handler = h2c.NewHandler(handler, &http2.Server{})
    }
    handler = otelHandler(handler)  // OpenTelemetry instrumentation
    
    // 8. Create HTTP server
    server := &http.Server{Handler: handler}
    
    return &Registry{
        app:    app,
        config: config,
        server: server,
        quit:   make(chan os.Signal, 1),
    }, nil
}
```

## HTTP Request Processing Pipeline

### Application Creation: `handlers.NewApp()`

```go
func NewApp(ctx context.Context, config *configuration.Configuration) *App {
    app := &App{
        Config:  config,
        Context: ctx,
        router:  v2.RouterWithPrefix(config.HTTP.Prefix),  // Gorilla mux router
        isCache: config.Proxy.RemoteURL != "",
    }
    
    // 1. Register route handlers
    app.register(v2.RouteNameBase, func(ctx *Context, r *http.Request) http.Handler {
        return http.HandlerFunc(apiBase)  // Simple /v2/ endpoint
    })
    app.register(v2.RouteNameManifest, manifestDispatcher)
    app.register(v2.RouteNameCatalog, catalogDispatcher)
    app.register(v2.RouteNameTags, tagsDispatcher)
    app.register(v2.RouteNameBlob, blobDispatcher)
    app.register(v2.RouteNameBlobUpload, blobUploadDispatcher)
    app.register(v2.RouteNameBlobUploadChunk, blobUploadDispatcher)
    
    // 2. Create storage driver
    app.driver, err = factory.Create(app, config.Storage.Type(), storageParams)
    
    // 3. Apply storage middleware
    app.driver, err = applyStorageMiddleware(app, app.driver, config.Middleware["storage"])
    
    // 4. Configure registry instance
    if app.registry == nil {
        app.registry, err = storage.NewRegistry(app.Context, app.driver, options...)
    }
    
    // 5. Apply registry middleware
    app.registry, err = applyRegistryMiddleware(app, app.registry, app.driver, config.Middleware["registry"])
    
    // 6. Configure authentication
    if authType != "" && !strings.EqualFold(authType, "none") {
        accessController, err := auth.GetAccessController(config.Auth.Type(), config.Auth.Parameters())
        app.accessController = accessController
    }
    
    // 7. Configure as proxy cache (optional)
    if config.Proxy.RemoteURL != "" {
        app.registry, err = proxy.NewRegistryPullThroughCache(ctx, app.registry, app.driver, config.Proxy)
    }
    
    return app
}
```

### Request Routing: Route Registration

```go
func (app *App) register(routeName string, dispatch dispatchFunc) {
    handler := app.dispatcher(dispatch)
    
    // Add Prometheus instrumentation
    if app.Config.HTTP.Debug.Prometheus.Enabled {
        namespace := metrics.NewNamespace(prometheus.NamespacePrefix, "http", nil)
        httpMetrics := namespace.NewDefaultHttpMetrics(strings.Replace(routeName, "-", "_", -1))
        handler = metrics.InstrumentHandler(httpMetrics, handler)
    }
    
    app.router.GetRoute(routeName).Handler(handler)
}
```

### Request Dispatcher: `app.dispatcher()`

```go
func (app *App) dispatcher(dispatch dispatchFunc) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // 1. Set custom headers
        for headerName, headerValues := range app.Config.HTTP.Headers {
            for _, value := range headerValues {
                w.Header().Add(headerName, value)
            }
        }
        
        // 2. Create request context
        context := app.context(w, r)
        
        // 3. Defer error handling
        defer func() {
            if context.Errors.Len() > 0 {
                _ = errcode.ServeJSON(w, context.Errors)
                app.logError(context, context.Errors)
            }
        }()
        
        // 4. Authorization check
        if err := app.authorized(w, r, context); err != nil {
            dcontext.GetLogger(context).Warnf("error authorizing context: %v", err)
            return
        }
        
        // 5. Repository resolution (if needed)
        if app.nameRequired(r) {
            nameRef, err := reference.WithName(getName(context))
            repository, err := app.registry.Repository(context, nameRef)
            
            // Apply repository middleware
            context.Repository, err = applyRepoMiddleware(app, context.Repository, app.Config.Middleware["repository"])
        }
        
        // 6. Execute route handler
        dispatch(context, r).ServeHTTP(w, r)
    })
}
```

### Authorization Flow: `app.authorized()`

```go
func (app *App) authorized(w http.ResponseWriter, r *http.Request, context *Context) error {
    repo := getName(context)
    
    if app.accessController == nil {
        return nil  // No auth configured
    }
    
    var accessRecords []auth.Access
    
    if repo != "" {
        // Repository-specific access
        accessRecords = appendAccessRecords(accessRecords, r.Method, repo)
        
        // Handle blob mounting from another repository
        if fromRepo := r.FormValue("from"); fromRepo != "" {
            accessRecords = appendAccessRecords(accessRecords, http.MethodGet, fromRepo)
        }
    } else {
        // Catalog access
        accessRecords = appendCatalogAccessRecord(accessRecords, r)
    }
    
    // Call access controller
    grant, err := app.accessController.Authorized(r.WithContext(context.Context), accessRecords...)
    if err != nil {
        switch err := err.(type) {
        case auth.Challenge:
            err.SetHeaders(r, w)  // WWW-Authenticate header
            if err := errcode.ServeJSON(w, errcode.ErrorCodeUnauthorized.WithDetail(accessRecords)); err != nil {
                dcontext.GetLogger(context).Errorf("error serving error json: %v", err)
            }
        default:
            w.WriteHeader(http.StatusBadRequest)
        }
        return err
    }
    
    // Store user and resources in context
    ctx := withUser(context.Context, grant.User)
    ctx = withResources(ctx, grant.Resources)
    context.Context = ctx
    
    return nil
}
```

## Storage Layer Architecture

### Storage Driver Interface: `storagedriver.StorageDriver`

```go
type StorageDriver interface {
    Name() string
    GetContent(ctx context.Context, path string) ([]byte, error)
    PutContent(ctx context.Context, path string, content []byte) error
    Reader(ctx context.Context, path string, offset int64) (io.ReadCloser, error)
    Writer(ctx context.Context, path string, append bool) (FileWriter, error)
    Stat(ctx context.Context, path string) (FileInfo, error)
    List(ctx context.Context, path string) ([]string, error)
    Move(ctx context.Context, sourcePath string, destPath string) error
    Delete(ctx context.Context, path string) error
    RedirectURL(r *http.Request, path string) (string, error)
    Walk(ctx context.Context, path string, f WalkFn, options ...func(*WalkOptions)) error
}
```

### Driver Factory Pattern: `factory.Create()`

```go
func Create(ctx context.Context, driverName string, parameters map[string]interface{}) (storagedriver.StorageDriver, error) {
    driverFactory, ok := driverFactories[driverName]
    if !ok {
        return nil, InvalidStorageDriverError{driverName}
    }
    return driverFactory.Create(ctx, parameters)
}

// Driver registration (happens in init() functions)
func Register(name string, factory StorageDriverFactory) {
    if factory == nil {
        panic("Must not provide nil StorageDriverFactory")
    }
    defer driverFactory.Unlock()
    driverFactory.Lock()
    if _, registered := driverFactories[name]; registered {
        panic(fmt.Sprintf("StorageDriverFactory named %s already registered", name))
    }
    driverFactories[name] = factory
}
```

### Storage Registry: `storage.NewRegistry()`

```go
func NewRegistry(ctx context.Context, driver storagedriver.StorageDriver, options ...RegistryOption) (distribution.Namespace, error) {
    bs := &blobStore{
        driver:    driver,
        pm:        defaultPathMapper,
        statter:   cache.NewCachedBlobStatter(cache.NewMemoryBlobDescriptorCacheProvider(), statter),
    }

    registry := &registry{
        driver:      driver,
        blobStore:   bs,
        statter:     statter,
        tagService:  &tagService{blobStore: bs},
        blobServer:  &blobServer{driver: driver, statter: statter, pathFn: bs.path},
    }

    for _, option := range options {
        if err := option(registry); err != nil {
            return nil, err
        }
    }

    return registry, nil
}
```

### Path Structure for Storage

The registry uses a well-defined path structure for content-addressable storage:

```
<root>/v2/
├── blobs/
│   └── <algorithm>/<first_two_hex>/<full_digest>/data
└── repositories/
    └── <name>/
        ├── _layers/
        │   └── <algorithm>/<hex_digest>/link
        ├── _manifests/
        │   ├── revisions/<algorithm>/<hex_digest>/link
        │   └── tags/<tag>/current/link
        └── _uploads/<id>/
            ├── data
            ├── hashstates/<algorithm>/<offset>
            └── startedat
```

## Authentication and Authorization System

### Access Controller Interface

```go
type AccessController interface {
    Authorized(r *http.Request, access ...Access) (*Grant, error)
}

type Access struct {
    Resource Resource
    Action   string
}

type Resource struct {
    Type  string
    Class string
    Name  string
}

type Grant struct {
    User      UserInfo
    Resources []Resource
}
```

### Token Authentication Flow

```go
func (t *accessController) Authorized(req *http.Request, accessRecords ...auth.Access) (*auth.Grant, error) {
    // 1. Extract Bearer token from Authorization header
    token := ""
    if auth := req.Header.Get("Authorization"); auth != "" {
        token = strings.TrimPrefix(auth, "Bearer ")
    }
    
    // 2. Parse and verify JWT token
    verifiedToken, err := t.verifyTokenAuth(req.Context(), token, claims)
    
    // 3. Validate claims (issuer, audience, expiration)
    if err := t.checkClaims(req, claims); err != nil {
        return nil, err
    }
    
    // 4. Check scope permissions
    for _, access := range accessRecords {
        if !t.scopeMatches(claims.Access, access) {
            return nil, &auth.Challenge{
                Realm:   t.realm,
                Service: t.service,
                Scope:   scopeString(access),
            }
        }
    }
    
    // 5. Return grant with authorized resources
    return &auth.Grant{
        User: auth.UserInfo{Name: claims.Subject},
        Resources: authorizedResources,
    }, nil
}
```

### HTPasswd Authentication Flow

```go
func (ac *accessController) Authorized(req *http.Request, accessRecords ...auth.Access) (*auth.Grant, error) {
    // 1. Extract Basic Auth credentials
    username, password, ok := req.BasicAuth()
    if !ok {
        return nil, &challenge{realm: ac.realm}
    }
    
    // 2. Check file modification and reload if needed
    if err := ac.refreshHtpasswdFile(); err != nil {
        return nil, err
    }
    
    // 3. Validate password against bcrypt hash
    if err := ac.validateCredentials(username, password); err != nil {
        return nil, &challenge{realm: ac.realm}
    }
    
    // 4. Return grant for authenticated user
    return &auth.Grant{
        User: auth.UserInfo{Name: username},
        Resources: allRequestedResources,
    }, nil
}
```

## Manifest and Blob Operations

### Blob Upload Flow

#### 1. Upload Initiation (`POST /v2/{name}/blobs/uploads/`)

```go
func blobUploadDispatcher(ctx *Context, r *http.Request) http.Handler {
    return &blobUploadHandler{
        Context: ctx,
        UUID:    getUploadUUID(ctx),
    }
}

func (buh *blobUploadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    switch r.Method {
    case http.MethodPost:
        return buh.StartBlobUpload(w, r)
    case http.MethodGet:
        return buh.GetUploadStatus(w, r)
    case http.MethodPatch:
        return buh.PatchBlobData(w, r)
    case http.MethodPut:
        return buh.PutBlobUploadComplete(w, r)
    case http.MethodDelete:
        return buh.DeleteBlobUpload(w, r)
    }
}

func (buh *blobUploadHandler) StartBlobUpload(w http.ResponseWriter, r *http.Request) {
    // 1. Handle blob mounting from another repository
    fromRepo := r.FormValue("from")
    if fromRepo != "" {
        return buh.mountBlob(w, r, fromRepo)
    }
    
    // 2. Create new upload session
    blobs := buh.Repository.Blobs(buh)
    upload, err := blobs.Create(buh)
    
    // 3. Set upload URL and UUID in response headers
    buh.Upload = upload
    w.Header().Set("Location", buh.urlBuilder.BuildBlobUploadURL(buh.Repository.Named(), upload.ID()))
    w.Header().Set("Range", "0-0")
    w.WriteHeader(http.StatusAccepted)
}
```

#### 2. Chunked Upload (`PATCH /v2/{name}/blobs/uploads/{uuid}`)

```go
func (buh *blobUploadHandler) PatchBlobData(w http.ResponseWriter, r *http.Request) {
    // 1. Validate Content-Range header
    contentRange := r.Header.Get("Content-Range")
    if contentRange != "" {
        startByte, endByte, err := parseContentRange(contentRange)
        if err != nil {
            buh.Errors = append(buh.Errors, v2.ErrorCodeRangeInvalid.WithDetail(err))
            return
        }
    }
    
    // 2. Write data to upload session
    nn, err := buh.Upload.ReadFrom(r.Body)
    if err != nil {
        buh.Errors = append(buh.Errors, errcode.ErrorCodeUnknown.WithDetail(err))
        return
    }
    
    // 3. Set response headers with current upload status
    w.Header().Set("Location", buh.urlBuilder.BuildBlobUploadURL(buh.Repository.Named(), buh.Upload.ID()))
    w.Header().Set("Range", fmt.Sprintf("0-%d", buh.Upload.Size()-1))
    w.WriteHeader(http.StatusAccepted)
}
```

#### 3. Upload Completion (`PUT /v2/{name}/blobs/uploads/{uuid}?digest={digest}`)

```go
func (buh *blobUploadHandler) PutBlobUploadComplete(w http.ResponseWriter, r *http.Request) {
    // 1. Extract digest from query parameter
    dgstStr := r.FormValue("digest")
    if dgstStr == "" {
        buh.Errors = append(buh.Errors, v2.ErrorCodeDigestInvalid.WithDetail("digest missing"))
        return
    }
    
    dgst, err := digest.Parse(dgstStr)
    if err != nil {
        buh.Errors = append(buh.Errors, v2.ErrorCodeDigestInvalid.WithDetail(err))
        return
    }
    
    // 2. Read any remaining data
    if r.ContentLength > 0 {
        nn, err := buh.Upload.ReadFrom(r.Body)
        if err != nil {
            buh.Errors = append(buh.Errors, errcode.ErrorCodeUnknown.WithDetail(err))
            return
        }
    }
    
    // 3. Commit upload with digest verification
    desc, err := buh.Upload.Commit(buh, distribution.Descriptor{Digest: dgst})
    if err != nil {
        switch err := err.(type) {
        case distribution.ErrBlobInvalidDigest:
            buh.Errors = append(buh.Errors, v2.ErrorCodeDigestInvalid.WithDetail(err))
        default:
            buh.Errors = append(buh.Errors, errcode.ErrorCodeUnknown.WithDetail(err))
        }
        return
    }
    
    // 4. Set response headers
    w.Header().Set("Location", buh.urlBuilder.BuildBlobURL(buh.Repository.Named(), desc.Digest))
    w.Header().Set("Content-Length", "0")
    w.Header().Set("Docker-Content-Digest", desc.Digest.String())
    w.WriteHeader(http.StatusCreated)
}
```

### Blob Download Flow

```go
func (bh *blobHandler) GetBlob(w http.ResponseWriter, r *http.Request) {
    // 1. Get blob descriptor
    blobs := bh.Repository.Blobs(bh)
    desc, err := blobs.Stat(bh, bh.Digest)
    if err != nil {
        if err == distribution.ErrBlobUnknown {
            bh.Errors = append(bh.Errors, v2.ErrorCodeBlobUnknown.WithDetail(bh.Digest))
        } else {
            bh.Errors = append(bh.Errors, errcode.ErrorCodeUnknown.WithDetail(err))
        }
        return
    }
    
    // 2. Handle redirects from storage backend
    if redirectURL, err := blobs.ServeBlob(bh, w, r, desc.Digest); err != nil {
        if redirectURL != "" {
            http.Redirect(w, r, redirectURL, http.StatusTemporaryRedirect)
            return
        }
        bh.Errors = append(bh.Errors, errcode.ErrorCodeUnknown.WithDetail(err))
        return
    }
    
    // 3. Set response headers
    w.Header().Set("Docker-Content-Digest", desc.Digest.String())
    w.Header().Set("Content-Type", desc.MediaType)
    w.Header().Set("Content-Length", fmt.Sprint(desc.Size))
}
```

### Manifest Upload Flow

```go
func (imh *imageManifestHandler) PutImageManifest(w http.ResponseWriter, r *http.Request) {
    // 1. Determine manifest media type
    mediaType := ""
    if v := r.Header.Get("Content-Type"); v != "" {
        mediaType = v
    }
    
    // 2. Read manifest content
    var jsonBuf bytes.Buffer
    if err := copyFullPayload(r, &jsonBuf, imh, "image manifest PUT"); err != nil {
        imh.Errors = append(imh.Errors, errcode.ErrorCodeUnknown.WithDetail(err))
        return
    }
    
    // 3. Unmarshal manifest based on media type
    var manifest distribution.Manifest
    switch mediaType {
    case schema2.MediaTypeManifest:
        var sm schema2.Manifest
        if err := json.Unmarshal(jsonBuf.Bytes(), &sm); err != nil {
            imh.Errors = append(imh.Errors, v2.ErrorCodeManifestInvalid.WithDetail(err))
            return
        }
        manifest = &sm
        
    case v1.MediaTypeImageManifest:
        var om ocischema.Manifest
        if err := json.Unmarshal(jsonBuf.Bytes(), &om); err != nil {
            imh.Errors = append(imh.Errors, v2.ErrorCodeManifestInvalid.WithDetail(err))
            return
        }
        manifest = &om
    }
    
    // 4. Validate manifest dependencies
    if err := imh.validateManifest(manifest); err != nil {
        imh.Errors = append(imh.Errors, err)
        return
    }
    
    // 5. Store manifest
    manifests, err := imh.Repository.Manifests(imh)
    desc, err := manifests.Put(imh, manifest, distribution.WithTag(imh.Tag))
    if err != nil {
        imh.Errors = append(imh.Errors, errcode.ErrorCodeUnknown.WithDetail(err))
        return
    }
    
    // 6. Set response headers
    w.Header().Set("Location", imh.urlBuilder.BuildManifestURL(imh.Repository.Named(), desc.Digest))
    w.Header().Set("Docker-Content-Digest", desc.Digest.String())
    w.WriteHeader(http.StatusCreated)
}
```

### Manifest Download Flow

```go
func (imh *imageManifestHandler) GetImageManifest(w http.ResponseWriter, r *http.Request) {
    // 1. Content negotiation based on Accept header
    supportsSchema2 := supportsManifestSchema2(r)
    supportsOCI := supportsOCIManifests(r)
    
    // 2. Get manifest service
    manifests, err := imh.Repository.Manifests(imh)
    if err != nil {
        imh.Errors = append(imh.Errors, err)
        return
    }
    
    // 3. Retrieve manifest by tag or digest
    var manifest distribution.Manifest
    if imh.Tag != "" {
        tags := imh.Repository.Tags(imh)
        desc, err := tags.Get(imh, imh.Tag)
        if err != nil {
            imh.Errors = append(imh.Errors, v2.ErrorCodeManifestUnknown.WithDetail(err))
            return
        }
        manifest, err = manifests.Get(imh, desc.Digest)
    } else {
        manifest, err = manifests.Get(imh, imh.Digest)
    }
    
    // 4. Handle manifest schema conversion if needed
    manifest = imh.convertManifestSchema(manifest, supportsSchema2, supportsOCI)
    
    // 5. Serialize and serve manifest
    ct, p, err := manifest.Payload()
    if err != nil {
        imh.Errors = append(imh.Errors, errcode.ErrorCodeUnknown.WithDetail(err))
        return
    }
    
    w.Header().Set("Content-Type", ct)
    w.Header().Set("Content-Length", fmt.Sprint(len(p)))
    w.Header().Set("Docker-Content-Digest", imh.Digest.String())
    w.Write(p)
}
```

## Key Design Patterns

### 1. Factory Pattern
Used extensively for storage drivers, authentication providers, and middleware:

```go
// Driver factory registration
func init() {
    factory.Register("filesystem", &filesystemDriverFactory{})
}

// Access controller factory
func GetAccessController(name string, options map[string]interface{}) (AccessController, error) {
    if initFunc, exists := accessControllers[name]; exists {
        return initFunc(options)
    }
    return nil, fmt.Errorf("no access controller registered with name: %s", name)
}
```

### 2. Middleware Pattern
Chainable middleware for storage drivers, registry, and repositories:

```go
func applyStorageMiddleware(ctx context.Context, driver storagedriver.StorageDriver, middlewares []configuration.Middleware) (storagedriver.StorageDriver, error) {
    for _, mw := range middlewares {
        smw, err := storagemiddleware.Get(ctx, mw.Name, mw.Options, driver)
        if err != nil {
            return nil, fmt.Errorf("unable to configure storage middleware (%s): %v", mw.Name, err)
        }
        driver = smw
    }
    return driver, nil
}
```

### 3. Context Pattern
Extensive use of Go's context for request scoping, cancellation, and value passing:

```go
func (app *App) context(w http.ResponseWriter, r *http.Request) *Context {
    ctx := r.Context()
    ctx = dcontext.WithVars(ctx, r)
    ctx = dcontext.WithLogger(ctx, dcontext.GetLogger(ctx,
        "vars.name",
        "vars.reference", 
        "vars.digest",
        "vars.uuid"))

    context := &Context{
        App:     app,
        Context: ctx,
    }
    return context
}
```

### 4. Functional Options Pattern
Used for registry configuration:

```go
type RegistryOption func(*registry) error

func EnableDelete(registry *registry) error {
    registry.deleteEnabled = true
    return nil
}

func NewRegistry(ctx context.Context, driver storagedriver.StorageDriver, options ...RegistryOption) (distribution.Namespace, error) {
    registry := &registry{...}
    
    for _, option := range options {
        if err := option(registry); err != nil {
            return nil, err
        }
    }
    
    return registry, nil
}
```

## Function Call Flows

### Complete Request Flow Example: GET /v2/library/nginx/manifests/latest

```
1. main() 
   ├── registry.RootCmd.Execute()
   ├── ServeCmd.Run()
   ├── NewRegistry()
   │   ├── handlers.NewApp()
   │   │   ├── v2.RouterWithPrefix()
   │   │   ├── app.register() for each route
   │   │   ├── factory.Create() for storage driver
   │   │   ├── storage.NewRegistry()
   │   │   └── auth.GetAccessController()
   │   ├── configureLogging()
   │   ├── health.Handler()
   │   └── otelHandler()
   └── registry.ListenAndServe()

2. HTTP Request Processing:
   ├── app.ServeHTTP()
   │   ├── dcontext.WithRequest()
   │   ├── dcontext.WithResponseWriter()
   │   └── app.router.ServeHTTP()
   └── app.dispatcher()
       ├── app.context()
       ├── app.authorized()
       │   ├── getName()
       │   ├── appendAccessRecords()
       │   ├── app.accessController.Authorized()
       │   └── withUser(), withResources()
       ├── app.registry.Repository()
       ├── applyRepoMiddleware()
       └── manifestDispatcher()

3. Manifest Handler:
   ├── imageManifestHandler.ServeHTTP()
   ├── imageManifestHandler.GetImageManifest()
   │   ├── supportsManifestSchema2()
   │   ├── imh.Repository.Manifests()
   │   ├── imh.Repository.Tags().Get()
   │   ├── manifests.Get()
   │   ├── imh.convertManifestSchema()
   │   └── manifest.Payload()
   └── HTTP Response with manifest JSON
```

### Storage Driver Call Flow: Blob Retrieval

```
1. blobHandler.GetBlob()
   ├── bh.Repository.Blobs()
   ├── blobs.Stat()
   │   ├── linkedBlobStore.Stat()
   │   ├── blobStore.statter.Stat()
   │   └── driver.Stat()
   └── blobs.ServeBlob()
       ├── blobServer.ServeBlob()
       ├── driver.RedirectURL() (if supported)
       └── driver.Reader() → http.ServeContent()
```

This comprehensive analysis covers the complete architecture and function flows of the Distribution registry, from initialization through request processing to storage operations.

<function_calls>
<invoke name="TodoWrite">
<parameter name="todos">[{"id": "1", "content": "Analyze main entry point and server initialization", "status": "completed", "priority": "high"}, {"id": "2", "content": "Examine HTTP request routing and handler registration", "status": "completed", "priority": "high"}, {"id": "3", "content": "Study storage layer architecture and driver interface", "status": "completed", "priority": "high"}, {"id": "4", "content": "Analyze authentication and authorization flow", "status": "completed", "priority": "medium"}, {"id": "5", "content": "Document manifest handling and blob operations", "status": "completed", "priority": "high"}, {"id": "6", "content": "Create comprehensive technical analysis document", "status": "completed", "priority": "high"}]
