package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"

	platformv1alpha1 "github.com/syntasso/kratix/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	yamlsig "sigs.k8s.io/yaml"
)

var l = log.New(os.Stderr, "", 0)

func main() {
	var resourcesPath string
	var promisePath string
	flag.StringVar(&resourcesPath, "k8s-resources-directory", "", "Absolute Path of k8s resources to build workerClusterResources from")
	flag.StringVar(&promisePath, "promise", "", "Absolute path of Promise to insert workerClusterResources into")
	flag.Parse()

	if resourcesPath == "" {
		fmt.Println("Must provide -k8s-resources-directory")
		os.Exit(1)
	}

	if promisePath == "" {
		fmt.Println("Must provide -promise")
		os.Exit(1)
	}

	//Read Resoures
	files, err := ioutil.ReadDir(resourcesPath)
	if err != nil {
		fmt.Println("Error reading resourcesPath: " + resourcesPath)
		os.Exit(1)
	}

	resources := []platformv1alpha1.WorkerClusterResource{}

	for _, fileInfo := range files {
		fileName := filepath.Join(resourcesPath, fileInfo.Name())

		file, _ := os.Open(fileName)

		decoder := yaml.NewYAMLOrJSONDecoder(file, 2048)
		for {
			us := &unstructured.Unstructured{}
			err := decoder.Decode(&us)

			if us == nil {
				continue
			} else if err == io.EOF {
				break
			} else {
				resources = append(resources, platformv1alpha1.WorkerClusterResource{Unstructured: *us})
			}
		}
	}

	//Read Promise
	promiseFile, _ := os.ReadFile(promisePath)
	promise := platformv1alpha1.Promise{}
	err = yaml.Unmarshal(promiseFile, &promise)
	//If there's an error unmarshalling from the template, log to stderr so it doesn't silently go to the promise
	if err != nil {
		l.Println(err.Error())
		os.Exit(1)
	}
	promise.Spec.WorkerClusterResources = resources

	fmt.Println(marshallWithWorkerClusterResourcesAtEndOfSpec(promise))
}

func marshallWithWorkerClusterResourcesAtEndOfSpec(promise platformv1alpha1.Promise) string {
	uniqueKey := "zWorkerClusterResources"

	//replace workerClusterResources key with zWorkerClusterResources
	promiseBytes := marshal(promise)
	//this only matches with the top level workerClusterResources keys, won't reorder workerClusterResources inside a BBP
	r := regexp.MustCompile("\n  workerClusterResources:\n")
	yamlWithZKey := r.ReplaceAll(promiseBytes, []byte(fmt.Sprintf("\n  %s:\n", uniqueKey)))

	//Unmarshal the yaml and remarshal it so zWorkerClusterResources comes after xaasCRD/RequestPipeline
	i := make(map[string]interface{})
	err := yaml.Unmarshal(yamlWithZKey, &i)
	if err != nil {
		l.Println(err.Error())
		os.Exit(1)
	}

	//Marshal again and rename zWorkerClusterResources to workerClusterResources
	promiseBytes = marshal(i)
	r = regexp.MustCompile(fmt.Sprintf("\n  %s:\n", uniqueKey))
	formattedYAML := r.ReplaceAll(promiseBytes, []byte("\n  workerClusterResources:\n"))
	return string(formattedYAML)
}

func marshal(promise interface{}) []byte {
	bytes, err := yamlsig.Marshal(promise)
	if err != nil {
		l.Println(err.Error())
		os.Exit(1)
	}
	return bytes
}
