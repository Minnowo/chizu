; Patterns earlier in the file win when several match the same node.

(class_declaration name: (_) @name) @definition.class
(class name: (_) @name) @definition.class

(method_definition name: [(property_identifier) (private_property_identifier)] @name) @definition.method

(function_declaration name: (identifier) @name) @definition.function
(generator_function_declaration name: (identifier) @name) @definition.function

(lexical_declaration
  (variable_declarator
    name: (identifier) @name
    value: [(arrow_function) (function_expression) (generator_function)]) @definition.function)
(variable_declaration
  (variable_declarator
    name: (identifier) @name
    value: [(arrow_function) (function_expression) (generator_function)]) @definition.function)

(assignment_expression
  left: (member_expression property: (property_identifier) @name)
  right: [(arrow_function) (function_expression)]) @definition.function
