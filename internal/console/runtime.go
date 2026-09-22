package console

import (
	"fmt"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

func InstallConnection(registry *provider.Registry, factory provider.ProviderFactory, connection Connection) error {
	configs := connection.ModelConfigs()
	deployments := make([]*provider.Deployment, 0, len(configs))
	for index, model := range configs {
		upstream, err := factory.Create(model)
		if err != nil {
			return fmt.Errorf("model %q: %w", model.Name, err)
		}
		deployments = append(deployments, &provider.Deployment{
			ID:            deploymentID(connection.ID, index),
			ModelName:     model.Name,
			ProviderName:  model.ProviderProfile,
			ProviderModel: model.ProviderModel,
			Provider:      upstream,
			IsEU:          model.IsEU,
			AuthMode:      model.AuthMode,
			BillingMode:   model.BillingMode,
		})
	}
	for index, model := range configs {
		registry.AddDeployment(model.Name, deployments[index])
	}
	return nil
}

func RemoveConnection(registry *provider.Registry, connection Connection) {
	for index, model := range connection.Models {
		registry.RemoveDeployment(model.Name, deploymentID(connection.ID, index))
	}
}

func deploymentID(connectionID string, modelIndex int) string {
	return fmt.Sprintf("byok-%s-%d", connectionID, modelIndex)
}
