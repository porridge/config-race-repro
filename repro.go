package main

import (
	"flag"
	"fmt"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/kubectl/pkg/cmd/util"
	"os"
	"sync"
)

var cloneConfig = flag.Bool("clone-config", false, "Whether the helm-operator-plugins rest client getter should return a copy of the config.")

func main() {
	useHelmGetter := flag.Bool("use-helm-operator-getter", true, "Whether to use helm-operator-plugins rest client getter.")
	parallelism := flag.Int("parallelism", 2, "Number of concurrent goroutines to use.")
	flag.Parse()

	// This is how kubectl and the helm tool create their RESTClientGetter
	k8sFlagsRESTClientGetter := genericclioptions.NewConfigFlags(true)
	// This is roughly how helm-operator-plugins library creates its RESTClientGetter
	// See https://github.com/operator-framework/helm-operator-plugins/blob/a307065f8d96cb3bb805c75f90d6aea43fd32709/pkg/client/restclientgetter.go#L41-L52
	helmOperatorRESTClientGetter := makeHelmOperatorRESTClientGetter(k8sFlagsRESTClientGetter)

	if *useHelmGetter {
		fmt.Println("using helm-operator-plugins rest client getter")
	} else {
		fmt.Println("using k8s CLI runtime client getter directly")
	}
	var wg sync.WaitGroup
	for i := 0; i < *parallelism; i++ {
		wg.Add(1)
		go func() {
			if *useHelmGetter {
				iterateUsingResourceBuilder(helmOperatorRESTClientGetter)
			} else {
				iterateUsingResourceBuilder(k8sFlagsRESTClientGetter)
			}
			wg.Done()
		}()
	}
	wg.Wait()
}

func makeHelmOperatorRESTClientGetter(flags *genericclioptions.ConfigFlags) genericclioptions.RESTClientGetter {
	restConfig, err := flags.ToRESTConfig()
	if err != nil {
		panic(err)
	}
	restMapper, err := flags.ToRESTMapper()
	if err != nil {
		panic(err)
	}
	return newRESTClientGetter(restConfig, restMapper, "default")
}

func iterateUsingResourceBuilder(clientGetter genericclioptions.RESTClientGetter) {
	f := util.NewFactory(clientGetter)
	fileName := "objects.yaml"
	file, err := os.Open(fileName)
	if err != nil {
		panic(err)
	}
	// This simulates the logic in helm kube client Build() method.
	// See https://github.com/helm/helm/blob/663a896f4a815053445eec4153677ddc24a0a361/pkg/kube/client.go#L185-L203
	_, err = f.NewBuilder().ContinueOnError().Flatten().Unstructured().Stream(file, fileName).Do().Infos()
	if err != nil {
		panic(err)
	}
}
