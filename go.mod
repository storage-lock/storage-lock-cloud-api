module github.com/storage-lock/storage-lock-cloud-api

go 1.23

require (
	github.com/gin-gonic/gin v1.9.1
	github.com/storage-lock/go-memory-storage v0.0.1
	github.com/storage-lock/go-storage v0.1.0
	github.com/storage-lock/go-storage-lock v0.1.0
)

require (
	github.com/bytedance/sonic v1.9.1 // indirect
	github.com/chenzhuoyu/base64x v0.0.0-20221115062448-fe3a3abad311 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/gabriel-vasile/mimetype v1.4.2 // indirect
	github.com/gin-contrib/sse v0.1.0 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/go-playground/validator/v10 v10.14.0 // indirect
	github.com/goccy/go-json v0.10.2 // indirect
	github.com/golang-infrastructure/go-goroutine-id v0.0.0-20230917075314-f1af231c0b06 // indirect
	github.com/golang-infrastructure/go-iterator v0.0.0-20230524171120-56988a9b127c // indirect
	github.com/golang-infrastructure/go-pointer v0.0.5 // indirect
	github.com/golang-infrastructure/go-reflect-utils v0.0.0-20221130143747-965ef2eb09c3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/cpuid/v2 v2.2.4 // indirect
	github.com/leodido/go-urn v1.2.4 // indirect
	github.com/mattn/go-isatty v0.0.19 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/pelletier/go-toml/v2 v2.0.8 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/storage-lock/go-events v0.0.5 // indirect
	github.com/storage-lock/go-storage-events v0.0.6 // indirect
	github.com/storage-lock/go-utils v0.0.2 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	github.com/ugorji/go/codec v1.2.11 // indirect
	golang.org/x/arch v0.3.0 // indirect
	golang.org/x/crypto v0.9.0 // indirect
	golang.org/x/net v0.10.0 // indirect
	golang.org/x/sys v0.8.0 // indirect
	golang.org/x/text v0.9.0 // indirect
	google.golang.org/protobuf v1.30.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	github.com/storage-lock/go-memory-storage => ../go-memory-storage
	github.com/storage-lock/go-storage => ../go-storage
	github.com/storage-lock/go-storage-lock => ../go-storage-lock
)

exclude github.com/ugorji/go v0.0.0-20171122102828-84cb69a8af83
