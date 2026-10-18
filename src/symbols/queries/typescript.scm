; Used for both .ts and .tsx. Patterns earlier in the file win when several
; match the same node.

(class_declaration name: (_) @name) @definition.class
(abstract_class_declaration name: (_) @name) @definition.class
(class name: (_) @name) @definition.class
(interface_declaration name: (_) @name) @definition.interface
(type_alias_declaration name: (_) @name) @definition.type
(enum_declaration name: (_) @name) @definition.enum
(internal_module name: (_) @name) @definition.module

(method_definition name: [(property_identifier) (private_property_identifier)] @name) @definition.method
(method_signature name: (property_identifier) @name) @definition.method
(abstract_method_signature name: (property_identifier) @name) @definition.method

(function_declaration name: (identifier) @name) @definition.function
(function_signature name: (identifier) @name) @definition.function
(generator_function_declaration name: (identifier) @name) @definition.function

(lexical_declaration
  (variable_declarator
    name: (identifier) @name
    value: [(arrow_function) (function_expression) (generator_function)]) @definition.function)
(variable_declaration
  (variable_declarator
    name: (identifier) @name
    value: [(arrow_function) (function_expression) (generator_function)]) @definition.function)
