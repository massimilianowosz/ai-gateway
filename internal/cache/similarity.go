package cache

import "math"

// CosineSimilarity computes the cosine similarity between two vectors.
// Returns a value in [-1, 1] where 1 means identical direction.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

// DualScore computes the weighted combination of query and context similarity.
// score = alpha * sim(query_a, query_b) + beta * sim(ctx_a, ctx_b)
// If either context embedding is nil/empty, only query similarity is used.
func DualScore(queryA, ctxA, queryB, ctxB []float32, alpha, beta float64) float64 {
	querySim := CosineSimilarity(queryA, queryB)

	if len(ctxA) == 0 || len(ctxB) == 0 {
		return querySim
	}

	ctxSim := CosineSimilarity(ctxA, ctxB)
	return alpha*querySim + beta*ctxSim
}
