binary:
	go build -o bin/mqgen github.com/AdityaVallabh/swagger_meqa/meqa/mqgen
	go build -o bin/mqgo github.com/AdityaVallabh/swagger_meqa/meqa/mqgo

test:
	go test github.com/AdityaVallabh/swagger_meqa/meqa/mqgen
	go test github.com/AdityaVallabh/swagger_meqa/meqa/mqgo
	