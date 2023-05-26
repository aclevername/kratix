{
  "apiVersion": "test.kratix.io/v1alpha1",
  "kind": "bash",
  "metadata": {
    "annotations": {
      "kubectl.kubernetes.io/last-applied-configuration": "{\"apiVersion\":\"test.kratix.io/v1alpha1\",\"kind\":\"bash\",\"metadata\":{\"annotations\":{},\"name\":\"example-rr\",\"namespace\":\"default\"},\"spec\":{\"container0Cmd\":\"echo \\"configmap: multi-container-config\\" \u003e\u003e /input/container-0.txt\n\",\"container1Cmd\":\"kubectl create configmap $(yq '.configmap' /input/container-0.txt) --dry-run=client -oyaml \u003e /output/configmap.yaml\n\"}}
"
    },
    "creationTimestamp": "2023-05-26T15:21:34Z",
    "generation": 1,
    "managedFields": [
      {
        "apiVersion": "test.kratix.io/v1alpha1",
        "fieldsType": "FieldsV1",
        "fieldsV1": {
          "f:metadata": {
            "f:annotations": {
              ".": {},
              "f:kubectl.kubernetes.io/last-applied-configuration": {}
            }
          },
          "f:spec": {
            ".": {},
            "f:container0Cmd": {},
            "f:container1Cmd": {}
          }
        },
        "manager": "kubectl-client-side-apply",
        "operation": "Update",
        "time": "2023-05-26T15:21:34Z"
      }
    ],
    "name": "example-rr",
    "namespace": "default",
    "uid": "0db5adf2-f126-4faa-95b4-8d727b366b5d"
  },
  "spec": {
    "container0Cmd": "echo \"configmap: multi-container-config\" >> /input/container-0.txt
",
    "container1Cmd": "kubectl create configmap $(yq '.configmap' /input/container-0.txt) --dry-run=client -oyaml > /output/configmap.yaml
"
  }
}
