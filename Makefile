.DEFAULT_GOAL = apply

DOCKER_REGISTRY = dwflynn
IMAGE_NAME = arb
IMAGE_TAG = 1.0.0

tools/ko = tools/bin/ko
tools/bin/%: tools/src/%/go.mod tools/src/%/pin.go
	cd $(<D) && GOOS= GOARCH= go build -o $(abspath $@) $$(sed -En 's,^import "(.*)".*,\1,p' pin.go)

tools: $(tools/ko)

apply: tools
	KO_DOCKER_REPO=$(DOCKER_REGISTRY) $(tools/ko) apply -f arb.yaml

push: tools
	docker tag $$($(tools/ko) publish --local .) $(DOCKER_REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)
	docker push $(DOCKER_REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)
.PHONY: push

demo: apply
	kubectl apply -f arblogger.yaml
	kubectl apply -f logsvc.yaml
	kubectl apply -f quote.yaml
	kubectl apply -f ratelimit.yaml
