package extensionrelease

import (
	"encoding/json"
	"fmt"
	"path"
)

// RecipeDirectory is the repository directory whose subdirectories are the
// extensions; a subdirectory with a recipe is a release extension.
const RecipeDirectory = "extensions"

// RecipePath is the slash-separated repository path of the recipe that makes
// the named extension a release extension.
func RecipePath(name string) string {
	return path.Join(RecipeDirectory, name, "recipe.json")
}

// RecipeRepository returns the OCI repository a release extension recipe
// names. Release publication pushes the extension's bundles there, so it is
// also where a release's published bundles are read.
func RecipeRepository(recipe []byte) (string, error) {
	var fields struct {
		Repository string `json:"repository"`
	}
	if err := json.Unmarshal(recipe, &fields); err != nil {
		return "", fmt.Errorf("decode release extension recipe: %w", err)
	}
	if err := ValidateRepository(fields.Repository); err != nil {
		return "", err
	}
	return fields.Repository, nil
}
