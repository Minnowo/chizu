; Patterns earlier in the file win when several match the same node.

(function_declaration
  name: (method_index_expression table: (_) @parent method: (identifier) @name)) @definition.method
(function_declaration
  name: (dot_index_expression table: (_) @parent field: (identifier) @name)) @definition.function
(function_declaration name: (identifier) @name) @definition.function
