.PHONY: build run clean docker-build docker-push

IMAGE_NAME ?= castai-pdb-controller
IMAGE_TAG ?= latest

build:
	go build -o castai-pdb-controller-bin ./cmd

run: build
	./castai-pdb-controller-bin

clean:
	rm -f castai-pdb-controller-bin

docker-build:
	docker build -t $(IMAGE_NAME):$(IMAGE_TAG) .

docker-push: docker-build
	docker push $(IMAGE_NAME):$(IMAGE_TAG)

mod-tidy:
	go mod tidy
