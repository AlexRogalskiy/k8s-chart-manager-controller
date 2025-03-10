NAMESPACE  := logicmonitor
REPOSITORY := chart-manager-controller
VERSION    := 0.1.0-alpha.0

all:
	docker build --rm --build-arg VERSION=$(VERSION) --build-arg CI=$(CI) -t $(NAMESPACE)/$(REPOSITORY):latest .
	docker run --rm -v "$(shell pwd)":/out --entrypoint=cp $(NAMESPACE)/$(REPOSITORY):latest /tmp/api.pb.go /out/api
	docker run --rm -v "$(shell pwd)":/out --entrypoint=cp $(NAMESPACE)/$(REPOSITORY):latest /tmp/zz_generated.deepcopy.go /out/pkg/apis/v1alpha1/
	docker tag $(NAMESPACE)/$(REPOSITORY):latest $(NAMESPACE)/$(REPOSITORY):$(VERSION)
