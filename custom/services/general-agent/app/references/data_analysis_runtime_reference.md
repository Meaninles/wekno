# Analysis runtime capabilities

Database tools return executor-owned dataset/query identity and result hashes. File tools additionally expose sheet names and physical cell coordinates through the original cell evidence table. Aggregations are traced to the dataset and executed query; the runtime does not invent cell mappings or semantically approve a result.

Use the returned structured chart handle when a chart is requested. Its axes and series refer to actual result columns. Source completeness, result truncation, chart validity and the interpretation of the result are separate facts.
