package service

// AppendCustomCodexModelsManifest retains the upstream manifest and its native
// model descriptors. Custom public names receive complete, conservative Codex
// descriptors, rather than being advertised as aliases of unrelated accounts.
func AppendCustomCodexModelsManifest(body []byte, modelIDs []string) ([]byte, error) {
	if len(modelIDs) == 0 {
		return body, nil
	}
	custom, err := BuildCodexModelsManifest(modelIDs)
	if err != nil {
		return nil, err
	}
	return mergeCodexModelsManifestBodies([][]byte{body, custom})
}
