dev:
	docker-compose -f docker-compose-dev.yaml up --build

prd:
	docker-compose up

build-client:
	docker build -t 0chaindev/blimp-clientapi:v0.0.4 -f ../client-api/Dockerfile .
