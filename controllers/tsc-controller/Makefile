.PHONY: build push lint clean

IMAGE ?= castai/tsc-controller
TAG ?= latest

build:
	docker build -t $(IMAGE):$(TAG) .

push: build
	docker push $(IMAGE):$(TAG)

lint:
	cd cmd && go vet ./...

clean:
	rm -rf cmd/tsc-controller