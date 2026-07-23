package policy

// Compiler keeps the layer ordering explicit: organization, project/repository,
// workflow/step, execution override, then the device-local ceiling.
type Compiler struct{}

func (Compiler) Compile(organization, project, workflow, execution, device Layer) (Snapshot, error) {
	return Compile(organization, project, workflow, execution, device)
}
