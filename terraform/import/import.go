package tfimport

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/onelogin/onelogin-go-sdk/pkg/utils"
	"github.com/onelogin/onelogin/terraform/importables"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
)

// ImportTFStateFromRemote writes the resource resourceDefinitions to main.tf and calls each
// resource's terraform import command to update tfstate
func ImportTFStateFromRemote(importable tfimportables.Importable) {
	p := filepath.Join("main.tf")
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		log.Fatalln("Unable to open main.tf ", err)
	}

	newResourceDefinitions := importable.ImportFromRemote()
	newResourceDefinitions, newProviderDefinitions := filterExistingDefinitions(f, newResourceDefinitions)

	if len(newResourceDefinitions) == 0 {
		fmt.Println("No new resources to import from remote")
		if err := f.Close(); err != nil {
			fmt.Println("Problem writing file", err)
		}
		os.Exit(0)
	}

	fmt.Printf("This will import %d resources. Do you want to continue? (y/n): ", len(newResourceDefinitions))
	input := bufio.NewScanner(os.Stdin)
	input.Scan()
	text := strings.ToLower(input.Text())
	if text != "y" && text != "yes" {
		fmt.Printf("User aborted operation!")
		if err := f.Close(); err != nil {
			fmt.Println("Problem writing file", err)
		}
		os.Exit(0)
	}

	defBuffer := createHCLDefinitionsBuffer(newResourceDefinitions, newProviderDefinitions)
	if _, err := f.Write(defBuffer); err != nil {
		log.Fatal("Problem creating import file", err)
	}

	log.Println("Initializing Terraform with 'terraform init'...")
	// #nosec G204
	if err := exec.Command("terraform", "init").Run(); err != nil {
		if err := f.Close(); err != nil {
			log.Fatal("Problem writing to main.tf", err)
		}
		log.Fatal("Problem executing terraform init", err)
	}

	for i, resourceDefinition := range newResourceDefinitions {
		arg1 := fmt.Sprintf("%s.%s", resourceDefinition.Type, resourceDefinition.Name)
		pos := strings.Index(arg1, "-")
		id := arg1[pos+1 : len(arg1)]
		// #nosec G204
		cmd := exec.Command("terraform", "import", arg1, id)
		log.Printf("Importing resource %d", i+1)
		if err := cmd.Run(); err != nil {
			log.Fatal("Problem executing terraform import", cmd.Args, err)
		}
	}

	state, err := collectState() // grab the state from tfstate
	if err != nil {
		if err := f.Close(); err != nil {
			log.Fatal("Problem writing to main.tf", err)
		}
		log.Fatalln("Unable to collect state from tfstate")
	}
	buffer := convertTFStateToHCL(state)
	f.Seek(0, 0) // go to the start of main.tf
	_, err = f.Write(buffer)
	if err != nil {
		if err := f.Close(); err != nil {
			fmt.Println("Problem writing file", err)
		}
		fmt.Println("ERROR Writing Final main.tf", err)
	}
	if err := f.Close(); err != nil {
		fmt.Println("Problem writing file", err)
	}
}

func collectState() (State, error) {
	state := State{}
	log.Println("Collecting State from tfstate File")
	data, err := ioutil.ReadFile(filepath.Join("terraform.tfstate"))
	if err != nil {
		log.Println(err)
		return state, errors.New("Unable to Read tfstate")
	}

	if err := json.Unmarshal(data, &state); err != nil {
		log.Println(err)
		return state, errors.New("Unable to Translate tfstate in Memory")
	}
	return state, nil
}

// compares incoming resources from remote to what is already defined in the main.tf
// file to prevent duplicate definitions which breaks terraform import
func filterExistingDefinitions(f io.Reader, resourceDefinitions []tfimportables.ResourceDefinition) ([]tfimportables.ResourceDefinition, []string) {
	searchCriteria := map[string]*regexp.Regexp{
		"provider": regexp.MustCompile(`(\w*provider\w*)\s(([a-zA-Z\_]*))\s\{`),
		"resource": regexp.MustCompile(`(\w*resource\w*)\s([a-zA-Z\_\-]*)\s([a-zA-Z\_\-]*[0-9]*)\s?\{`),
	}
	collection := make(map[string]map[string]int)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		t := scanner.Text()
		for regexName, r := range searchCriteria {
			if collection[regexName] == nil {
				collection[regexName] = make(map[string]int)
			}
			subStr := r.FindStringSubmatch(t)
			if len(subStr) > 0 {
				var definitionKey string
				if regexName == "provider" {
					definitionKey = fmt.Sprintf("%s", subStr[len(subStr)-1])
				}
				if regexName == "resource" {
					definitionKey = fmt.Sprintf("%s.%s", subStr[len(subStr)-2], subStr[len(subStr)-1])
				}
				collection[regexName][definitionKey]++
			}
		}
	}

	uniqueResourceDefinitions := []tfimportables.ResourceDefinition{}
	uniqueProviders := []string{}
	providerMap := map[string]int{}

	for _, resourceDefinition := range resourceDefinitions {
		providerMap[resourceDefinition.Provider]++
		if collection["resource"][fmt.Sprintf("%s.%s", resourceDefinition.Type, resourceDefinition.Name)] == 0 {
			uniqueResourceDefinitions = append(uniqueResourceDefinitions, resourceDefinition)
		}
	}

	for provider := range providerMap {
		if collection["provider"][provider] == 0 {
			uniqueProviders = append(uniqueProviders, provider)
		}
	}

	return uniqueResourceDefinitions, uniqueProviders
}

// in preparation for terraform import, appends empty resource definitions to the existing main.tf file
func createHCLDefinitionsBuffer(resourceDefinitions []tfimportables.ResourceDefinition, providerDefinitions []string) []byte {
	var builder strings.Builder
	for _, newProvider := range providerDefinitions {
		builder.WriteString(fmt.Sprintf("provider %s {\n\talias = \"%s\"\n}\n\n", newProvider, newProvider))
	}
	for _, resourceDefinition := range resourceDefinitions {
		builder.WriteString(fmt.Sprintf("resource %s %s {}\n", resourceDefinition.Type, resourceDefinition.Name))
	}
	return []byte(builder.String())
}

// takes the tfstate representations formats them as HCL and writes them to a bytes buffer
// so it can be flushed into main.tf
func convertTFStateToHCL(state State) []byte {
	var builder strings.Builder
	knownProviders := map[string]int{}

	log.Println("Assembling main.tf...")

	for _, resource := range state.Resources {
		providerDefinition := strings.Replace(resource.Provider, "provider.", "", 1)
		if knownProviders[providerDefinition] == 0 {
			knownProviders[providerDefinition]++
			builder.WriteString(fmt.Sprintf("provider %s {\n\talias = \"%s\"\n}\n\n", providerDefinition, providerDefinition))
		}
		for _, instance := range resource.Instances {
			builder.WriteString(fmt.Sprintf("resource %s %s {\n", resource.Type, resource.Name))
			builder.WriteString(fmt.Sprintf("\tprovider = %s\n", providerDefinition))
			sculptedData := sculpt(resource.Type, instance.Data)
			convertToHCLLine(sculptedData, 1, &builder)
			builder.WriteString("}\n\n")
		}
		builder.WriteString(string(resource.Content))
	}
	return []byte(builder.String())
}

func indent(level int) []byte {
	out := make([]byte, level)
	for i := 0; i < level; i++ {
		out[i] = byte('\t')
	}
	return out
}

// recursively converts a chunk of data from it's struct representation to its HCL representation
// and appends the "line" to a bytes buffer.
func convertToHCLLine(input interface{}, indentLevel int, builder *strings.Builder) {
	b, err := json.Marshal(input)
	if err != nil {
		log.Fatalln("unable to parse state to hcl")
	}
	var m map[string]interface{}
	json.Unmarshal(b, &m)

	for k, v := range m {
		switch reflect.TypeOf(v).Kind() {
		case reflect.String:
			builder.WriteString(fmt.Sprintf("%s%s = %q\n", indent(indentLevel), utils.ToSnakeCase(k), v))
		case reflect.Int, reflect.Int32, reflect.Float32, reflect.Float64, reflect.Bool:
			builder.WriteString(fmt.Sprintf("%s%s = %v\n", indent(indentLevel), utils.ToSnakeCase(k), v))
		case reflect.Array, reflect.Slice:
			sl := v.([]interface{})
			if len(sl) > 0 {
				switch reflect.TypeOf(sl[0]).Kind() {
				case reflect.Array, reflect.Slice, reflect.Map:
					for j := 0; j < len(sl); j++ {
						builder.WriteString(strings.ToLower(fmt.Sprintf("\n%s%s {\n", indent(indentLevel), utils.ToSnakeCase(k))))
						convertToHCLLine(sl[j], indentLevel+1, builder)
						builder.WriteString(fmt.Sprintf("%s}\n", indent(indentLevel)))
					}
				default:
					builder.WriteString(fmt.Sprintf("%s%s = [", indent(indentLevel), utils.ToSnakeCase(k)))
					for j := 0; j < len(sl); j++ {
						builder.WriteString(fmt.Sprintf("%q", sl[j]))
						if j < len(sl)-1 {
							builder.WriteString(",")
						}
					}
					builder.WriteString("]\n")
				}
			}
		default:
			fmt.Println("Unable to Determine Type")
		}
	}
}
