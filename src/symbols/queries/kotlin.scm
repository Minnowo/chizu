; Patterns earlier in the file win when several match the same node.

(class_body (function_declaration (simple_identifier) @name) @definition.method)
(function_declaration (simple_identifier) @name) @definition.function

(class_declaration "interface" (type_identifier) @name) @definition.interface
(class_declaration (type_identifier) @name) @definition.class
(object_declaration (type_identifier) @name) @definition.object
(type_alias (type_identifier) @name) @definition.type
