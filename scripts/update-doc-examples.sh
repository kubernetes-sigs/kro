#!/bin/bash

create_example() {
   out_file=$1
   position=$2
   readme_file=$3
   rgd_file=$4

   # Verify all parameters are valid
   if [ -z "$out_file" ] || [ -z "$position" ] || [ -z "$readme_file" ] || [ -z "$rgd_file" ]; then
       echo "Error: Missing required parameters"
       echo "Usage: create_example <out_file> <position> <readme_file> <rgd_file>"
       return 1
   fi


   cat > "$out_file" << EOF
---
sidebar_position: $position
---

$(cat $readme_file)

<details>
  <summary>ResourceGraphDefinition</summary>
  \`\`\`yaml title="rgd.yaml"
$(cat "$yaml_file")
  \`\`\`
</details>
EOF

}

update_gcp_example_docs() {
    # Create the GCP examples directory if it doesn't exist
    mkdir -p website/docs/examples/gcp
    # Initialize position counter
    position=405
    # Find all rgd.yaml files in examples/gcp directory and its subdirectories
    find examples/gcp -name "rgd.yaml" | while read -r yaml_file; do
        # Extract the directory name as the example name
        example_path=$(dirname "$yaml_file")
        dir_name=$(basename $example_path)
        readme_file=$example_path/README.md
        out_file="website/docs/examples/gcp/${dir_name}.md"
        
        # Convert directory name to title case (e.g., gke-cluster -> GKE Cluster)
        # title=$(echo "$dir_name" | sed -E 's/-/ /g' | awk '{for(i=1;i<=NF;i++)sub(/./,toupper(substr($i,1,1)),$i)}1')
        
        # copy all images
        cp $example_path/*.png website/docs/examples/gcp/ 2>/dev/null
        # Generate the markdown content
        create_example "$out_file" "$position" "$readme_file" "$yaml_file"
       
        # Increment position for next file
        ((position+=1))
        
        echo "Generated documentation for ${dir_name} at ${out_file}"
    done 
}

update_aws_example_docs() {
    echo "TODO: implement aws examples"
}
update_azure_example_docs() {
    echo "TODO: implement azure examples"
}
update_kubernetes_example_docs() {
    # Create the Kubernetes examples directory if it doesn't exist
    mkdir -p website/docs/examples/kubernetes
    # Initialize position counter
    position=705
    # Find all rgd.yaml files in examples/gcp directory and its subdirectories
    find examples/kubernetes -name "*rgd.yaml" | while read -r yaml_file; do
        # Extract the directory name as the example name
        example_path=$(dirname "$yaml_file")
        dir_name=$(basename $example_path)
        readme_file=$example_path/README.md
        out_file="website/docs/examples/kubernetes/${dir_name}.md"

        # Convert directory name to title case (e.g., k8s-cluster -> K8s Cluster)
        # title=$(echo "$dir_name" | sed -E 's/-/ /g' | awk '{for(i=1;i<=NF;i++)sub(/./,toupper(substr($i,1,1)),$i)}1')

        # copy all images
        cp $example_path/*.png website/docs/examples/kubernetes/ 2>/dev/null
        # Generate the markdown content
        create_example "$out_file" "$position" "$readme_file" "$yaml_file"

        # Increment position for next file
        ((position+=1))

        echo "Generated documentation for ${dir_name} at ${out_file}"
    done
}

update_gcp_example_docs
update_aws_example_docs
update_azure_example_docs
update_kubernetes_example_docs
