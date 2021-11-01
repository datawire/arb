.DEFAULT_GOAL = apply

DOCKER_REGISTRY = dwflynn
IMAGE_NAME = arb
IMAGE_TAG = 1.0.0

tools/ko = tools/bin/ko
tools/bin/%: tools/src/%/go.mod tools/src/%/pin.go
	cd $(<D) && GOOS= GOARCH= go build -o $(abspath $@) $$(sed -En 's,^import "(.*)".*,\1,p' pin.go)

apply: $(tools/ko)
	KO_DOCKER_REPO=$(DOCKER_REGISTRY) $(tools/ko) apply -f arb.yaml

push: $(tools/ko)
	docker tag $$($(tools/ko) publish --local .) $(DOCKER_REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)
	docker push $(DOCKER_REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)
.PHONY: push
